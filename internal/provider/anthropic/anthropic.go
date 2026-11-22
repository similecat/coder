package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"coder/internal/provider/sseutil"
	"coder/internal/types"
)

const (
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultAnthropicVersion = "2023-06-01"
	defaultMaxTokens        = 8192
)

// Provider implements types.ChatProvider for the Anthropic Messages SSE API (`/v1/messages`).
type Provider struct {
	profile types.ProviderProfile
	baseURL string
	apiKey  string
	version string
	client  *http.Client
}

var _ types.ChatProvider = (*Provider)(nil)

// New constructs a pure-Go Anthropic Messages SSE ChatProvider.
func New(profile types.ProviderProfile) (*Provider, error) {
	apiKey, err := sseutil.ResolveAPIKey(profile)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}

	version := strings.TrimSpace(profile.APIVersion)
	if version == "" {
		version = defaultAnthropicVersion
	}

	return &Provider{
		profile: profile,
		baseURL: baseURL,
		apiKey:  apiKey,
		version: version,
		client:  sseutil.NewHTTPClient(),
	}, nil
}

// Name returns the configured provider name (defaulting to "anthropic").
func (p *Provider) Name() string {
	if p.profile.Name != "" {
		return p.profile.Name
	}
	return string(types.ProviderTypeAnthropic)
}

// DefaultModel returns the configured default model ID.
func (p *Provider) DefaultModel() string {
	return p.profile.DefaultModel
}

// Close satisfies types.Provider.
func (p *Provider) Close() error {
	return nil
}

func (p *Provider) v1Endpoint(subpath string) string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + subpath
	}
	return p.baseURL + "/v1" + subpath
}

func (p *Provider) applyHeaders(req *http.Request, streaming bool) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", p.version)
	if p.apiKey != "" {
		req.Header.Set("x-api-key", p.apiKey)
	}
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	for k, v := range p.profile.Headers {
		req.Header.Set(k, v)
	}
}

type cacheControl struct {
	Type string `json:"type"`
}

type systemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type anthropicTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl *cacheControl  `json:"cache_control,omitempty"`
}

type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   *bool  `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      []systemBlock      `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
}

func ensureInputSchema(params map[string]any) map[string]any {
	if len(params) == 0 {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	if _, hasType := params["type"]; hasType {
		return params
	}
	cloned := make(map[string]any, len(params)+1)
	for k, v := range params {
		cloned[k] = v
	}
	cloned["type"] = "object"
	return cloned
}

func parseToolUseInput(raw json.RawMessage) map[string]any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err == nil && obj != nil {
		return obj
	}
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err == nil {
			if err := json.Unmarshal([]byte(inner), &obj); err == nil && obj != nil {
				return obj
			}
		}
	}
	return map[string]any{}
}

func (p *Provider) buildRequestPayload(req types.ChatRequest) anthropicRequest {
	model := req.Model
	if model == "" {
		model = p.profile.DefaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	var sysParts []string
	if req.SystemPrompt != "" {
		sysParts = append(sysParts, req.SystemPrompt)
	}

	var messages []anthropicMessage
	for _, m := range req.Messages {
		switch m.Role {
		case types.RoleSystem:
			if m.Content != "" {
				sysParts = append(sysParts, m.Content)
			}

		case types.RoleTool:
			isErr := m.IsError
			trBlock := contentBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
				IsError:   &isErr,
			}
			// Coalesce consecutive RoleTool results into a single "role": "user" message.
			if n := len(messages); n > 0 && messages[n-1].Role == "user" {
				messages[n-1].Content = append(messages[n-1].Content, trBlock)
			} else {
				messages = append(messages, anthropicMessage{
					Role:    "user",
					Content: []contentBlock{trBlock},
				})
			}

		case types.RoleUser:
			txtBlock := contentBlock{
				Type: "text",
				Text: m.Content,
			}
			if n := len(messages); n > 0 && messages[n-1].Role == "user" {
				messages[n-1].Content = append(messages[n-1].Content, txtBlock)
			} else {
				messages = append(messages, anthropicMessage{
					Role:    "user",
					Content: []contentBlock{txtBlock},
				})
			}

		case types.RoleAssistant:
			blocks := make([]contentBlock, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				blocks = append(blocks, contentBlock{
					Type: "text",
					Text: m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, contentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: parseToolUseInput(tc.Arguments),
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, contentBlock{Type: "text", Text: ""})
			}
			messages = append(messages, anthropicMessage{
				Role:    "assistant",
				Content: blocks,
			})
		}
	}

	var sysBlocks []systemBlock
	if sysText := strings.Join(sysParts, "\n\n"); sysText != "" {
		sysBlocks = []systemBlock{{
			Type:         "text",
			Text:         sysText,
			CacheControl: &cacheControl{Type: "ephemeral"},
		}}
	}

	var tools []anthropicTool
	if len(req.Tools) > 0 {
		tools = make([]anthropicTool, 0, len(req.Tools))
		for i, t := range req.Tools {
			at := anthropicTool{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: ensureInputSchema(t.Parameters),
			}
			if i == len(req.Tools)-1 {
				at.CacheControl = &cacheControl{Type: "ephemeral"}
			}
			tools = append(tools, at)
		}
	}

	return anthropicRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		System:      sysBlocks,
		Messages:    messages,
		Tools:       tools,
		Temperature: req.Temperature,
		Stream:      true,
	}
}

// StreamChat streams an Anthropic Messages API response over SSE with ephemeral prompt caching
// on system instructions and tool definitions.
func (p *Provider) StreamChat(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
	payload := p.buildRequestPayload(req)
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.v1Endpoint("/messages"), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create anthropic request: %w", err)
	}
	p.applyHeaders(httpReq, true)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	return parseAnthropicSSE(resp.Body, onEvent)
}

type sseEnvelope struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		StopReason *string         `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type     string          `json:"type"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Input    json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type toolBlockAccum struct {
	index        int
	id           string
	name         string
	initialInput json.RawMessage
	jsonBuf      strings.Builder
}

func parseAnthropicSSE(body io.Reader, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
	reader := sseutil.NewSSEReader(body)

	var (
		contentBuf   strings.Builder
		stopReason   string
		inTokens     int
		outTokens    int
		cachedTokens int
		seenUsage    bool
		emittedUsage bool
		toolBlocks   = make(map[int]*toolBlockAccum)
	)

	emit := func(ev types.StreamEvent) {
		if onEvent != nil {
			onEvent(ev)
		}
	}

	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read anthropic sse: %w", err)
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			continue
		}

		var env sseEnvelope
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			return nil, fmt.Errorf("decode anthropic sse %q: %w", ev.Event, err)
		}

		evType := ev.Event
		if evType == "" {
			evType = env.Type
		}

		switch evType {
		case "ping":
			continue

		case "error":
			msg := "unknown anthropic stream error"
			if env.Error != nil && env.Error.Message != "" {
				msg = env.Error.Message
			}
			streamErr := fmt.Errorf("anthropic stream error: %s", msg)
			emit(types.StreamEvent{Type: types.EventError, Err: streamErr})
			return nil, streamErr

		case "message_start":
			if env.Message != nil {
				if env.Message.StopReason != nil && *env.Message.StopReason != "" {
					stopReason = *env.Message.StopReason
				}
				if u := env.Message.Usage; u != nil {
					seenUsage = true
					inTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
					outTokens = u.OutputTokens
					cachedTokens = u.CacheReadInputTokens
				}
			}

		case "content_block_start":
			if cb := env.ContentBlock; cb != nil {
				switch cb.Type {
				case "text":
					if cb.Text != "" {
						contentBuf.WriteString(cb.Text)
						emit(types.StreamEvent{Type: types.EventTextDelta, Text: cb.Text})
					}
				case "thinking":
					if cb.Thinking != "" {
						emit(types.StreamEvent{Type: types.EventReasoningDelta, Text: cb.Thinking})
					}
				case "tool_use":
					toolBlocks[env.Index] = &toolBlockAccum{
						index:        env.Index,
						id:           cb.ID,
						name:         cb.Name,
						initialInput: cb.Input,
					}
				}
			}

		case "content_block_delta":
			if d := env.Delta; d != nil {
				switch d.Type {
				case "text_delta":
					if d.Text != "" {
						contentBuf.WriteString(d.Text)
						emit(types.StreamEvent{Type: types.EventTextDelta, Text: d.Text})
					}
				case "thinking_delta":
					if d.Thinking != "" {
						emit(types.StreamEvent{Type: types.EventReasoningDelta, Text: d.Thinking})
					}
				case "input_json_delta":
					tb, ok := toolBlocks[env.Index]
					if !ok {
						tb = &toolBlockAccum{index: env.Index}
						toolBlocks[env.Index] = tb
					}
					tb.jsonBuf.WriteString(d.PartialJSON)
				}
			}

		case "message_delta":
			if env.Delta != nil && env.Delta.StopReason != "" {
				stopReason = env.Delta.StopReason
			}
			if u := env.Usage; u != nil {
				seenUsage = true
				if u.OutputTokens > 0 {
					outTokens = u.OutputTokens
				}
				if u.CacheReadInputTokens > 0 {
					cachedTokens = u.CacheReadInputTokens
				}
				if totalIn := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; totalIn > inTokens {
					inTokens = totalIn
				}
				emittedUsage = true
				emit(types.StreamEvent{
					Type:         types.EventUsage,
					InputTokens:  inTokens,
					OutputTokens: outTokens,
					CachedTokens: cachedTokens,
				})
			}

		case "message_stop":
			if seenUsage && !emittedUsage {
				emittedUsage = true
				emit(types.StreamEvent{
					Type:         types.EventUsage,
					InputTokens:  inTokens,
					OutputTokens: outTokens,
					CachedTokens: cachedTokens,
				})
			}
		}
	}

	if seenUsage && !emittedUsage {
		emit(types.StreamEvent{
			Type:         types.EventUsage,
			InputTokens:  inTokens,
			OutputTokens: outTokens,
			CachedTokens: cachedTokens,
		})
	}

	var toolCalls []types.ToolCall
	if len(toolBlocks) > 0 {
		indices := make([]int, 0, len(toolBlocks))
		for idx := range toolBlocks {
			indices = append(indices, idx)
		}
		sort.Ints(indices)

		toolCalls = make([]types.ToolCall, 0, len(indices))
		for i, idx := range indices {
			tb := toolBlocks[idx]
			id := tb.id
			if id == "" {
				id = fmt.Sprintf("toolu_%d", i+1)
			}
			var rawArgs json.RawMessage
			if streamed := strings.TrimSpace(tb.jsonBuf.String()); streamed != "" {
				rawArgs = json.RawMessage(streamed)
			} else if initTrimmed := bytes.TrimSpace(tb.initialInput); len(initTrimmed) > 0 {
				rawArgs = json.RawMessage(initTrimmed)
			} else {
				rawArgs = json.RawMessage("{}")
			}
			toolCalls = append(toolCalls, types.ToolCall{
				ID:        id,
				Name:      tb.name,
				Arguments: rawArgs,
			})
		}
	}

	return &types.ChatResponse{
		Content:      contentBuf.String(),
		ToolCalls:    toolCalls,
		InputTokens:  inTokens,
		OutputTokens: outTokens,
		CachedTokens: cachedTokens,
		StopReason:   stopReason,
	}, nil
}

type anthropicModelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
}

// ListModels queries GET <BaseURL>/v1/models, falling back to DefaultModel / built-in Claude IDs if unavailable.
func (p *Provider) ListModels(ctx context.Context) ([]types.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reqCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := 15 * time.Second
		if p.profile.TimeoutSec > 0 {
			timeout = time.Duration(p.profile.TimeoutSec) * time.Second
		}
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.v1Endpoint("/models"), nil)
	if err != nil {
		return nil, err
	}
	p.applyHeaders(httpReq, false)

	resp, err := p.client.Do(httpReq)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			return nil, fmt.Errorf("anthropic models HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var parsed anthropicModelsResponse
			if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&parsed); err == nil && len(parsed.Data) > 0 {
				provName := p.Name()
				out := make([]types.ModelInfo, 0, len(parsed.Data))
				for _, m := range parsed.Data {
					if m.ID == "" {
						continue
					}
					name := m.DisplayName
					if name == "" {
						name = m.ID
					}
					out = append(out, types.ModelInfo{
						ID:          m.ID,
						Name:        name,
						Provider:    provName,
						ContextSize: 200000,
					})
				}
				if len(out) > 0 {
					return out, nil
				}
			}
		}
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	provName := p.Name()
	if p.profile.DefaultModel != "" {
		return []types.ModelInfo{{
			ID:          p.profile.DefaultModel,
			Name:        p.profile.DefaultModel,
			Provider:    provName,
			ContextSize: 200000,
		}}, nil
	}
	return []types.ModelInfo{
		{ID: "claude-sonnet-4-20250514", Name: "Claude Sonnet 4", Provider: provName, ContextSize: 200000},
		{ID: "claude-opus-4-20250514", Name: "Claude Opus 4", Provider: provName, ContextSize: 200000},
	}, nil
}
