package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"coder/internal/types"
)

func TestStreamChat_TextAndDeepSeekReasoningDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-deepseek-test" {
			t.Errorf("unexpected Authorization header: %q", got)
		}

		body, _ := io.ReadAll(r.Body)
		var reqMap map[string]any
		if err := json.Unmarshal(body, &reqMap); err != nil {
			t.Fatalf("invalid request JSON: %v", err)
		}
		if reqMap["stream"] != true {
			t.Errorf("expected stream=true, got %v", reqMap["stream"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me analyze "}}]}`,
			`data: {"choices":[{"index":0,"delta":{"reasoning":"the bug."}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"Fixed "}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"it!"},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":35,"prompt_tokens_details":{"cached_tokens":80}}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	p, err := New(types.ProviderProfile{
		Name:         "deepseek",
		Type:         types.ProviderTypeOpenAICompat,
		BaseURL:      srv.URL + "/v1",
		APIKey:       "sk-deepseek-test",
		DefaultModel: "deepseek-reasoner",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	var reasoning strings.Builder
	var text strings.Builder
	var usageEvents []types.StreamEvent

	resp, err := p.StreamChat(context.Background(), types.ChatRequest{
		SystemPrompt: "You are a Go expert.",
		Messages: []types.Message{
			{Role: types.RoleUser, Content: "Fix bug"},
		},
	}, func(ev types.StreamEvent) {
		switch ev.Type {
		case types.EventReasoningDelta:
			reasoning.WriteString(ev.Text)
		case types.EventTextDelta:
			text.WriteString(ev.Text)
		case types.EventUsage:
			usageEvents = append(usageEvents, ev)
		}
	})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	if reasoning.String() != "Let me analyze the bug." {
		t.Fatalf("unexpected reasoning: %q", reasoning.String())
	}
	if text.String() != "Fixed it!" || resp.Content != "Fixed it!" {
		t.Fatalf("unexpected content: streamed=%q resp=%q", text.String(), resp.Content)
	}
	if resp.InputTokens != 120 || resp.OutputTokens != 35 || resp.CachedTokens != 80 || resp.StopReason != "stop" {
		t.Fatalf("unexpected response metadata: %+v", resp)
	}
	if len(usageEvents) != 1 || usageEvents[0].CachedTokens != 80 {
		t.Fatalf("unexpected usage events: %+v", usageEvents)
	}
}

func TestStreamChat_MultiChunkToolCallsAndWireEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments any    `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}

		// Verify outbound assistant tool_calls[0].function.arguments is a JSON string per OpenAI spec!
		if len(payload.Messages) != 4 {
			t.Fatalf("expected 4 messages (system, user, assistant, tool), got %d", len(payload.Messages))
		}
		argStr, ok := payload.Messages[2].ToolCalls[0].Function.Arguments.(string)
		if !ok || argStr != `{"path":"go.mod"}` {
			t.Fatalf("expected function.arguments to be JSON string '{\"path\":\"go.mod\"}', got %T (%v)", payload.Messages[2].ToolCalls[0].Function.Arguments, payload.Messages[2].ToolCalls[0].Function.Arguments)
		}
		if payload.Messages[3].Role != "tool" || payload.Messages[3].ToolCallID != "call_prev" {
			t.Fatalf("unexpected tool result message: %+v", payload.Messages[3])
		}
		if len(payload.Tools) != 1 || payload.Tools[0].Type != "function" || payload.Tools[0].Function.Name != "edit_file" {
			t.Fatalf("unexpected tools payload: %+v", payload.Tools)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_edit_1","type":"function","function":{"name":"edit_file","arguments":""}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"main.go\","}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"line\":10}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
		}
	}))
	defer srv.Close()

	p, err := New(types.ProviderProfile{
		Name:         "openai",
		Type:         types.ProviderTypeOpenAICompat,
		BaseURL:      srv.URL,
		APIKey:       "sk-test",
		DefaultModel: "gpt-4.1",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	resp, err := p.StreamChat(context.Background(), types.ChatRequest{
		SystemPrompt: "System",
		Messages: []types.Message{
			{Role: types.RoleUser, Content: "Check go.mod and edit main.go"},
			{
				Role: types.RoleAssistant,
				ToolCalls: []types.ToolCall{{
					ID:        "call_prev",
					Name:      "read_file",
					Arguments: json.RawMessage(`{"path":"go.mod"}`),
				}},
			},
			{
				Role:       types.RoleTool,
				ToolCallID: "call_prev",
				Content:    "module coder",
			},
		},
		Tools: []types.ToolDefinition{{
			Name:        "edit_file",
			Description: "Edit file",
			Parameters:  map[string]any{"type": "object"},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 assembled tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_edit_1" || tc.Name != "edit_file" || string(tc.Arguments) != `{"path":"main.go","line":10}` {
		t.Fatalf("unexpected assembled ToolCall: %+v (args=%s)", tc, string(tc.Arguments))
	}
}

func TestStreamChat_OllamaToolCallsWithoutIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header for keyless local Ollama, got %q", auth)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Simulate Ollama emitting two tool calls without IDs (both using index 0)
		chunks := []string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}},{"index":0,"function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
		}
	}))
	defer srv.Close()

	p, err := New(types.ProviderProfile{
		Name:         "ollama",
		Type:         types.ProviderTypeOpenAICompat,
		BaseURL:      srv.URL + "/v1",
		DefaultModel: "qwen2.5-coder:14b",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	resp, err := p.StreamChat(context.Background(), types.ChatRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "Read both files"}},
	}, nil)
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}

	if len(resp.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %+v", len(resp.ToolCalls), resp.ToolCalls)
	}
	if resp.ToolCalls[0].ID != "call_1" || string(resp.ToolCalls[0].Arguments) != `{"path":"a.go"}` {
		t.Fatalf("unexpected tool call 0: %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID != "call_2" || string(resp.ToolCalls[1].Arguments) != `{"path":"b.go"}` {
		t.Fatalf("unexpected tool call 1: %+v", resp.ToolCalls[1])
	}
}

func TestStreamChat_FallbackWhenStreamOptionsReturns400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "stream_options") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: stream_options"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok-%d\"}}]}\n\ndata: [DONE]\n\n", n)
	}))
	defer srv.Close()

	p, err := New(types.ProviderProfile{
		Name:         "llama.cpp",
		Type:         types.ProviderTypeOpenAICompat,
		BaseURL:      srv.URL + "/v1",
		DefaultModel: "local-gguf",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	resp1, err := p.StreamChat(context.Background(), types.ChatRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	}, nil)
	if err != nil || resp1.Content != "ok-2" {
		t.Fatalf("expected retry ok-2, got resp=%+v err=%v", resp1, err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 HTTP attempts on first call, got %d", attempts.Load())
	}

	// Second call remembers stream_options is unsupported and succeeds in 1 round-trip!
	resp2, err := p.StreamChat(context.Background(), types.ChatRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi again"}},
	}, nil)
	if err != nil || resp2.Content != "ok-3" || attempts.Load() != 3 {
		t.Fatalf("expected cached stream_options=false on second call, attempts=%d resp=%+v err=%v", attempts.Load(), resp2, err)
	}
}

func TestAzureAndListModels(t *testing.T) {
	t.Run("Azure api-version query and api-key header + ListModels", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("api-version") != "2024-10-21" {
				t.Errorf("expected api-version=2024-10-21, got %q", r.URL.RawQuery)
			}
			if r.Header.Get("api-key") != "azure-secret" {
				t.Errorf("expected api-key header, got %q", r.Header.Get("api-key"))
			}
			if r.Header.Get("Authorization") != "" {
				t.Errorf("did not expect Authorization Bearer on Azure, got %q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o","context_length":128000},{"id":"o3-mini","max_model_len":200000}]}`))
		}))
		defer srv.Close()

		p, err := New(types.ProviderProfile{
			Name:       "azure",
			Type:       types.ProviderTypeOpenAICompat,
			BaseURL:    srv.URL + "/openai",
			APIKey:     "azure-secret",
			APIVersion: "2024-10-21",
		})
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}

		models, err := p.ListModels(context.Background())
		if err != nil || len(models) != 2 {
			t.Fatalf("ListModels failed: models=%+v err=%v", models, err)
		}
		if models[0].ID != "gpt-4o" || models[0].ContextSize != 128000 || models[1].ContextSize != 200000 {
			t.Fatalf("unexpected models: %+v", models)
		}
	})

	t.Run("ListModels falls back to DefaultModel when /models returns 404", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		p, err := New(types.ProviderProfile{
			Name:         "minimal-local",
			Type:         types.ProviderTypeOpenAICompat,
			BaseURL:      srv.URL + "/v1",
			DefaultModel: "qwen-local",
		})
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}

		models, err := p.ListModels(context.Background())
		if err != nil || len(models) != 1 || models[0].ID != "qwen-local" {
			t.Fatalf("expected fallback to qwen-local, got models=%+v err=%v", models, err)
		}
	})
}
