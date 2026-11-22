package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"coder/internal/provider/sseutil"
	"coder/internal/types"
)

// Provider implements types.ChatProvider over OpenAI-compatible HTTP SSE endpoints
// (Ollama, llama.cpp, LM Studio, vLLM, OpenAI, OpenRouter, DeepSeek, Azure OpenAI).
type Provider struct {
	profile              types.ProviderProfile
	baseURL              string
	apiKey               string
	client               *http.Client
	disableStreamOptions atomic.Bool

	modelMu   sync.Mutex
	autoModel string
}

var _ types.ChatProvider = (*Provider)(nil)

// New constructs an OpenAI-compatible streaming ChatProvider.
func New(profile types.ProviderProfile) (*Provider, error) {
	apiKey, err := sseutil.ResolveAPIKey(profile)
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/")
	if baseURL == "" {
		lowerName := strings.ToLower(profile.Name)
		switch {
		case strings.Contains(lowerName, "ollama"):
			baseURL = "http://127.0.0.1:11434/v1"
		case strings.Contains(lowerName, "lmstudio") || strings.Contains(lowerName, "lm-studio") || strings.Contains(lowerName, "lm studio"):
			baseURL = "http://127.0.0.1:1234/v1"
		case strings.Contains(lowerName, "llama") || strings.Contains(lowerName, "vllm"):
			baseURL = "http://127.0.0.1:8080/v1"
		case strings.Contains(lowerName, "openrouter"):
			baseURL = "https://openrouter.ai/api/v1"
		case strings.Contains(lowerName, "deepseek"):
			baseURL = "https://api.deepseek.com/v1"
		default:
			baseURL = "https://api.openai.com/v1"
		}
	}

	return &Provider{
		profile: profile,
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  sseutil.NewHTTPClient(),
	}, nil
}

// Name returns the configured provider name (defaulting to "openai_compat").
func (p *Provider) Name() string {
	if p.profile.Name != "" {
		return p.profile.Name
	}
	return string(types.ProviderTypeOpenAICompat)
}

// DefaultModel returns the configured default model ID.
func (p *Provider) DefaultModel() string {
	return p.profile.DefaultModel
}

// Close satisfies types.Provider (connections are pooled in the shared transport).
func (p *Provider) Close() error {
	return nil
}

func (p *Provider) endpointURL(path string) string {
	full := p.baseURL + path
	if p.profile.APIVersion == "" {
		return full
	}
	sep := "?"
	if strings.Contains(full, "?") {
		sep = "&"
	}
	return full + sep + "api-version=" + url.QueryEscape(p.profile.APIVersion)
}

func (p *Provider) applyHeaders(req *http.Request, streaming bool) {
	req.Header.Set("Content-Type", "application/json")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	if p.profile.APIVersion != "" {
		if p.apiKey != "" {
			req.Header.Set("api-key", p.apiKey)
		}
	} else if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	for k, v := range p.profile.Headers {
		req.Header.Set(k, v)
	}
}

type oaiChatRequest struct {
	Model              string            `json:"model"`
	Messages           []oaiMessage      `json:"messages"`
	Tools              []oaiTool         `json:"tools,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	MaxTokens          int               `json:"max_tokens,omitempty"`
	ReasoningEffort    string            `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]any    `json:"chat_template_kwargs,omitempty"`
	Stream             bool              `json:"stream"`
	StreamOptions      *oaiStreamOptions `json:"stream_options,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiFunctionCall `json:"function"`
}

type oaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string         `json:"type"`
	Function oaiFunctionDef `json:"function"`
}

type oaiFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

func strPtr(s string) *string {
	return &s
}

func normalizeArgumentsString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "{}"
	}
	if trimmed[0] == '"' {
		var unquoted string
		if err := json.Unmarshal(trimmed, &unquoted); err == nil {
			inner := strings.TrimSpace(unquoted)
			if inner == "" {
				return "{}"
			}
			if json.Valid([]byte(inner)) {
				return inner
			}
		}
	}
	return string(trimmed)
}

func ensureSchema(params map[string]any) map[string]any {
	if len(params) == 0 {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	return params
}

func (p *Provider) buildRequestPayload(req types.ChatRequest, includeStreamOpts bool) oaiChatRequest {
	model := req.Model
	if model == "" {
		model = p.profile.DefaultModel
	}

	msgs := make([]oaiMessage, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		msgs = append(msgs, oaiMessage{
			Role:    "system",
			Content: strPtr(req.SystemPrompt),
		})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case types.RoleSystem:
			msgs = append(msgs, oaiMessage{
				Role:    "system",
				Content: strPtr(m.Content),
			})
		case types.RoleUser:
			msgs = append(msgs, oaiMessage{
				Role:    "user",
				Content: strPtr(m.Content),
			})
		case types.RoleAssistant:
			om := oaiMessage{Role: "assistant"}
			if len(m.ToolCalls) == 0 || m.Content != "" {
				om.Content = strPtr(m.Content)
			}
			if len(m.ToolCalls) > 0 {
				om.ToolCalls = make([]oaiToolCall, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					om.ToolCalls = append(om.ToolCalls, oaiToolCall{
						ID:   tc.ID,
						Type: "function",
						Function: oaiFunctionCall{
							Name:      tc.Name,
							Arguments: normalizeArgumentsString(tc.Arguments),
						},
					})
				}
			}
			msgs = append(msgs, om)
		case types.RoleTool:
			msgs = append(msgs, oaiMessage{
				Role:       "tool",
				Content:    strPtr(m.Content),
				ToolCallID: m.ToolCallID,
			})
		}
	}

	var tools []oaiTool
	if len(req.Tools) > 0 {
		tools = make([]oaiTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, oaiTool{
				Type: "function",
				Function: oaiFunctionDef{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  ensureSchema(t.Parameters),
				},
			})
		}
	}

	effort := strings.TrimSpace(req.ReasoningEffort)
	var templateKwargs map[string]any
	if strings.EqualFold(effort, "none") || strings.EqualFold(effort, "off") {
		effort = "none"
		if sseutil.IsLocalProvider(p.profile) {
			templateKwargs = map[string]any{"enable_thinking": false}
		}
	}

	payload := oaiChatRequest{
		Model:              model,
		Messages:           msgs,
		Tools:              tools,
		Temperature:        req.Temperature,
		MaxTokens:          req.MaxTokens,
		ReasoningEffort:    effort,
		ChatTemplateKwargs: templateKwargs,
		Stream:             true,
	}
	if includeStreamOpts {
		payload.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	return payload
}

func isStreamOptionsError(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "stream_options") || strings.Contains(lower, "include_usage")
}

func (p *Provider) resolveAutoModel(ctx context.Context) (string, error) {
	p.modelMu.Lock()
	if p.autoModel != "" {
		m := p.autoModel
		p.modelMu.Unlock()
		return m, nil
	}
	p.modelMu.Unlock()

	models, err := p.ListModels(ctx)
	if err != nil || len(models) == 0 {
		return "", fmt.Errorf("provider %q has no model specified and /models discovery failed: %w", p.Name(), err)
	}
	chosen := models[0].ID
	for _, m := range models {
		lower := strings.ToLower(m.ID)
		if strings.Contains(lower, "embed") || strings.Contains(lower, "voice") ||
			strings.Contains(lower, "whisper") || strings.Contains(lower, "tts") ||
			strings.Contains(lower, "rerank") {
			continue
		}
		chosen = m.ID
		break
	}
	p.modelMu.Lock()
	p.autoModel = chosen
	p.modelMu.Unlock()
	return chosen, nil
}

func (p *Provider) doChatHTTP(ctx context.Context, payload oaiChatRequest) (*http.Response, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpointURL("/chat/completions"), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}
	p.applyHeaders(httpReq, true)
	return p.client.Do(httpReq)
}

// StreamChat streams an OpenAI-compatible chat completion over SSE, handling reasoning tokens,
// multi-chunk tool arguments, keyless local tool calls, and automatic fallback when stream_options is unsupported.
func (p *Provider) StreamChat(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
	if strings.TrimSpace(req.Model) == "" && strings.TrimSpace(p.profile.DefaultModel) == "" {
		resolved, err := p.resolveAutoModel(ctx)
		if err != nil {
			return nil, err
		}
		req.Model = resolved
	}
	useStreamOpts := !p.disableStreamOptions.Load()
	payload := p.buildRequestPayload(req, useStreamOpts)

	httpResp, err := p.doChatHTTP(ctx, payload)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode == http.StatusBadRequest && useStreamOpts {
		errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 64*1024))
		_ = httpResp.Body.Close()
		if isStreamOptionsError(errBody) {
			p.disableStreamOptions.Store(true)
			retryPayload := p.buildRequestPayload(req, false)
			httpResp, err = p.doChatHTTP(ctx, retryPayload)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("openai_compat HTTP 400: %s", strings.TrimSpace(string(errBody)))
		}
	}

	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 64*1024))
		return nil, fmt.Errorf("openai_compat HTTP %d: %s", httpResp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	return parseSSEStream(httpResp.Body, onEvent)
}

type oaiStreamChunk struct {
	Choices []struct {
		Index        int     `json:"index"`
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Role             string             `json:"role"`
			Content          string             `json:"content"`
			ReasoningContent string             `json:"reasoning_content"`
			Reasoning        string             `json:"reasoning"`
			ToolCalls        []oaiToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type oaiToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolCallAccum struct {
	id      string
	name    string
	argsBuf strings.Builder
}

func parseSSEStream(body io.Reader, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
	reader := sseutil.NewSSEReader(body)
	var (
		contentBuf strings.Builder
		stopReason string
		inTokens   int
		outTokens  int
		cachedTok  int
		toolMap    = make(map[int]*toolCallAccum)
		toolOrder  []int
		nextSynth  = 0
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
			return nil, fmt.Errorf("read sse stream: %w", err)
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("decode sse chunk: %w", err)
		}

		if chunk.Error != nil && chunk.Error.Message != "" {
			streamErr := fmt.Errorf("provider stream error: %s", chunk.Error.Message)
			emit(types.StreamEvent{Type: types.EventError, Err: streamErr})
			return nil, streamErr
		}

		for _, choice := range chunk.Choices {
			reasoning := choice.Delta.ReasoningContent
			if reasoning == "" {
				reasoning = choice.Delta.Reasoning
			}
			if reasoning != "" {
				emit(types.StreamEvent{
					Type: types.EventReasoningDelta,
					Text: reasoning,
				})
			}

			if choice.Delta.Content != "" {
				contentBuf.WriteString(choice.Delta.Content)
				emit(types.StreamEvent{
					Type: types.EventTextDelta,
					Text: choice.Delta.Content,
				})
			}

			for _, tc := range choice.Delta.ToolCalls {
				key := tc.Index
				acc, exists := toolMap[key]
				// Handle local models (e.g. Ollama) that emit multiple complete tool calls all with index=0.
				if exists && tc.Function.Name != "" && acc.name != "" {
					if (tc.ID != "" && acc.id != "" && tc.ID != acc.id) ||
						(acc.argsBuf.Len() > 0 && json.Valid([]byte(acc.argsBuf.String()))) {
						nextSynth--
						key = nextSynth
						exists = false
					}
				}
				if !exists {
					acc = &toolCallAccum{}
					toolMap[key] = acc
					toolOrder = append(toolOrder, key)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" && acc.name == "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.argsBuf.WriteString(tc.Function.Arguments)
				}
			}

			if choice.FinishReason != nil && *choice.FinishReason != "" {
				stopReason = *choice.FinishReason
			}
		}

		if chunk.Usage != nil {
			inTokens = chunk.Usage.PromptTokens
			outTokens = chunk.Usage.CompletionTokens
			if chunk.Usage.PromptTokensDetails != nil {
				cachedTok = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			emit(types.StreamEvent{
				Type:         types.EventUsage,
				InputTokens:  inTokens,
				OutputTokens: outTokens,
				CachedTokens: cachedTok,
			})
		}
	}

	var toolCalls []types.ToolCall
	if len(toolOrder) > 0 {
		toolCalls = make([]types.ToolCall, 0, len(toolOrder))
		for i, k := range toolOrder {
			acc := toolMap[k]
			id := acc.id
			if id == "" {
				id = fmt.Sprintf("call_%d", i+1)
			}
			rawArgs := strings.TrimSpace(acc.argsBuf.String())
			if rawArgs == "" {
				rawArgs = "{}"
			}
			toolCalls = append(toolCalls, types.ToolCall{
				ID:        id,
				Name:      acc.name,
				Arguments: json.RawMessage(rawArgs),
			})
		}
	}

	return &types.ChatResponse{
		Content:      contentBuf.String(),
		ToolCalls:    toolCalls,
		InputTokens:  inTokens,
		OutputTokens: outTokens,
		CachedTokens: cachedTok,
		StopReason:   stopReason,
	}, nil
}

type oaiModelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
		MaxModelLen   int    `json:"max_model_len"`
	} `json:"data"`
	Models []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"models"`
}

// ListModels queries GET <BaseURL>/models, falling back to DefaultModel if the server
// does not implement /models (common on minimal local inference servers).
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

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.endpointURL("/models"), nil)
	if err != nil {
		return nil, err
	}
	p.applyHeaders(httpReq, false)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return p.fallbackModels(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, fmt.Errorf("openai_compat models HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return p.fallbackModels(fmt.Errorf("openai_compat models HTTP %d", resp.StatusCode))
	}

	var parsed oaiModelsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&parsed); err != nil {
		return p.fallbackModels(err)
	}

	provName := p.Name()
	out := make([]types.ModelInfo, 0, len(parsed.Data)+len(parsed.Models))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		ctxLen := m.ContextLength
		if ctxLen == 0 {
			ctxLen = m.MaxModelLen
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, types.ModelInfo{
			ID:          m.ID,
			Name:        name,
			Provider:    provName,
			ContextSize: ctxLen,
		})
	}
	if len(out) == 0 {
		for _, m := range parsed.Models {
			id := m.ID
			if id == "" {
				id = m.Name
			}
			if id == "" {
				continue
			}
			name := m.Name
			if name == "" {
				name = id
			}
			out = append(out, types.ModelInfo{
				ID:       id,
				Name:     name,
				Provider: provName,
			})
		}
	}
	if len(out) == 0 {
		return p.fallbackModels(nil)
	}
	return out, nil
}

func (p *Provider) fallbackModels(origErr error) ([]types.ModelInfo, error) {
	if p.profile.DefaultModel != "" {
		return []types.ModelInfo{{
			ID:       p.profile.DefaultModel,
			Name:     p.profile.DefaultModel,
			Provider: p.Name(),
		}}, nil
	}
	if origErr != nil {
		return nil, origErr
	}
	return []types.ModelInfo{}, nil
}
