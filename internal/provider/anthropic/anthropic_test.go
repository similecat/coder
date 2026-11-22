package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"coder/internal/types"
)

func TestStreamChat_CacheControlHeadersAndToolResultCoalescing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected /v1/messages, got %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant-secret" {
			t.Errorf("expected x-api-key=sk-ant-secret, got %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("expected anthropic-version=2023-06-01, got %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected Content-Type=application/json, got %q", got)
		}

		rawBody, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
			System []struct {
				Type         string            `json:"type"`
				Text         string            `json:"text"`
				CacheControl map[string]string `json:"cache_control"`
			} `json:"system"`
			Tools []struct {
				Name         string            `json:"name"`
				InputSchema  map[string]any    `json:"input_schema"`
				CacheControl map[string]string `json:"cache_control"`
			} `json:"tools"`
			Messages []struct {
				Role    string           `json:"role"`
				Content []map[string]any `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(rawBody, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}

		// 1. Verify system prompt block has cache_control={"type":"ephemeral"}
		if len(req.System) != 1 || req.System[0].Text != "Follow repository style." || req.System[0].CacheControl["type"] != "ephemeral" {
			t.Fatalf("unexpected system payload: %+v", req.System)
		}

		// 2. Verify ONLY the last tool has cache_control={"type":"ephemeral"}
		if len(req.Tools) != 2 {
			t.Fatalf("expected 2 tools, got %d", len(req.Tools))
		}
		if len(req.Tools[0].CacheControl) != 0 {
			t.Fatalf("expected first tool to have no cache_control, got %+v", req.Tools[0].CacheControl)
		}
		if req.Tools[1].CacheControl["type"] != "ephemeral" {
			t.Fatalf("expected last tool to have ephemeral cache_control, got %+v", req.Tools[1].CacheControl)
		}

		// 3. Verify messages:
		//    [0] user text
		//    [1] assistant text + 2 tool_use blocks with JSON object input
		//    [2] coalesced SINGLE user message containing 2 tool_result blocks!
		if len(req.Messages) != 3 {
			t.Fatalf("expected 3 messages after coalescing consecutive RoleTool messages, got %d: %s", len(req.Messages), string(rawBody))
		}
		assistantMsg := req.Messages[1]
		if assistantMsg.Role != "assistant" || len(assistantMsg.Content) != 3 {
			t.Fatalf("unexpected assistant message: %+v", assistantMsg)
		}
		toolUseBlock := assistantMsg.Content[1]
		if toolUseBlock["type"] != "tool_use" || toolUseBlock["id"] != "toolu_1" || toolUseBlock["name"] != "read_file" {
			t.Fatalf("unexpected tool_use block: %+v", toolUseBlock)
		}
		inputObj, ok := toolUseBlock["input"].(map[string]any)
		if !ok || inputObj["path"] != "a.go" {
			t.Fatalf("expected tool_use input to be JSON object with path=a.go, got %T (%v)", toolUseBlock["input"], toolUseBlock["input"])
		}

		coalescedUser := req.Messages[2]
		if coalescedUser.Role != "user" || len(coalescedUser.Content) != 2 {
			t.Fatalf("expected single coalesced user message with 2 tool_result blocks, got %+v", coalescedUser)
		}
		tr0, tr1 := coalescedUser.Content[0], coalescedUser.Content[1]
		if tr0["type"] != "tool_result" || tr0["tool_use_id"] != "toolu_1" || tr0["is_error"] != false {
			t.Fatalf("unexpected tool_result[0]: %+v", tr0)
		}
		if tr1["type"] != "tool_result" || tr1["tool_use_id"] != "toolu_2" || tr1["is_error"] != true {
			t.Fatalf("unexpected tool_result[1]: %+v", tr1)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":40,\"cache_read_input_tokens\":460,\"output_tokens\":1}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Checking b.go error...\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Let me create \"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"b.go.\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_3\",\"name\":\"write_file\",\"input\":{}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"b.go\\\",\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"content\\\":\\\"package main\\\"}\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":55}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, ev := range events {
			fmt.Fprint(w, ev)
		}
	}))
	defer srv.Close()

	p, err := New(types.ProviderProfile{
		Name:         "anthropic",
		Type:         types.ProviderTypeAnthropic,
		BaseURL:      srv.URL,
		APIKey:       "sk-ant-secret",
		DefaultModel: "claude-sonnet-4-20250514",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	var streamedText strings.Builder
	var streamedThinking strings.Builder
	var usageEvents []types.StreamEvent

	resp, err := p.StreamChat(context.Background(), types.ChatRequest{
		SystemPrompt: "Follow repository style.",
		Messages: []types.Message{
			{Role: types.RoleUser, Content: "Inspect a.go and b.go"},
			{
				Role:    types.RoleAssistant,
				Content: "Reading both files.",
				ToolCalls: []types.ToolCall{
					{ID: "toolu_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)},
					{ID: "toolu_2", Name: "read_file", Arguments: json.RawMessage(`{"path":"b.go"}`)},
				},
			},
			{Role: types.RoleTool, ToolCallID: "toolu_1", Content: "package main", IsError: false},
			{Role: types.RoleTool, ToolCallID: "toolu_2", Content: "file not found", IsError: true},
		},
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "Read file", Parameters: map[string]any{"type": "object"}},
			{Name: "write_file", Description: "Write file", Parameters: map[string]any{"type": "object"}},
		},
	}, func(ev types.StreamEvent) {
		switch ev.Type {
		case types.EventTextDelta:
			streamedText.WriteString(ev.Text)
		case types.EventReasoningDelta:
			streamedThinking.WriteString(ev.Text)
		case types.EventUsage:
			usageEvents = append(usageEvents, ev)
		}
	})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	if streamedThinking.String() != "Checking b.go error..." {
		t.Fatalf("unexpected thinking delta: %q", streamedThinking.String())
	}
	if streamedText.String() != "Let me create b.go." || resp.Content != "Let me create b.go." {
		t.Fatalf("unexpected text: streamed=%q resp=%q", streamedText.String(), resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "toolu_3" || tc.Name != "write_file" || string(tc.Arguments) != `{"path":"b.go","content":"package main"}` {
		t.Fatalf("unexpected assembled tool call: %+v (args=%s)", tc, string(tc.Arguments))
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("expected stop_reason=tool_use, got %q", resp.StopReason)
	}
	if resp.InputTokens != 500 || resp.CachedTokens != 460 || resp.OutputTokens != 55 {
		t.Fatalf("unexpected token counts: in=%d cached=%d out=%d", resp.InputTokens, resp.CachedTokens, resp.OutputTokens)
	}
	if len(usageEvents) != 1 || usageEvents[0].OutputTokens != 55 || usageEvents[0].CachedTokens != 460 {
		t.Fatalf("expected 1 final usage event, got %+v", usageEvents)
	}
}

func TestAnthropic_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("expected /v1/models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-20250514","display_name":"Claude Sonnet 4"}]}`))
	}))
	defer srv.Close()

	p, err := New(types.ProviderProfile{
		Name:    "anthropic",
		Type:    types.ProviderTypeAnthropic,
		BaseURL: srv.URL,
		APIKey:  "sk-ant-test",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	models, err := p.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "claude-sonnet-4-20250514" || models[0].Name != "Claude Sonnet 4" {
		t.Fatalf("unexpected ListModels result: %+v err=%v", models, err)
	}
}

