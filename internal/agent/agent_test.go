package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"coder/internal/types"
)

type mockTool struct {
	name     string
	category types.ToolCategory
	fn       func(ctx context.Context, args json.RawMessage) (types.ToolResult, error)
}

func (m *mockTool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        m.name,
		Description: "mock " + m.name,
		Parameters:  map[string]any{"type": "object"},
		Category:    m.category,
	}
}

func (m *mockTool) Execute(ctx context.Context, args json.RawMessage) (types.ToolResult, error) {
	return m.fn(ctx, args)
}

type scriptedChatProvider struct {
	steps []*types.ChatResponse
	idx   int
	reqs  []types.ChatRequest
}

func (p *scriptedChatProvider) Name() string         { return "mock-chat" }
func (p *scriptedChatProvider) DefaultModel() string { return "mock-v1" }
func (p *scriptedChatProvider) ListModels(context.Context) ([]types.ModelInfo, error) {
	return []types.ModelInfo{{ID: "mock-v1", Provider: "mock-chat"}}, nil
}
func (p *scriptedChatProvider) Close() error { return nil }

func (p *scriptedChatProvider) StreamChat(_ context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
	p.reqs = append(p.reqs, req)
	resp := p.steps[p.idx]
	p.idx++
	if resp.Content != "" {
		onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: resp.Content})
	}
	return resp, nil
}

func TestParallelReadToolsAndSequentialEditExecution(t *testing.T) {
	var activeReads atomic.Int32
	var maxConcurrentReads atomic.Int32

	readTool := &mockTool{
		name:     "read_file",
		category: types.ToolCategoryRead,
		fn: func(ctx context.Context, args json.RawMessage) (types.ToolResult, error) {
			cur := activeReads.Add(1)
			for {
				prev := maxConcurrentReads.Load()
				if cur <= prev || maxConcurrentReads.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			activeReads.Add(-1)
			return types.ToolResult{Content: "contents:" + string(args)}, nil
		},
	}

	editTool := &mockTool{
		name:     "edit_file",
		category: types.ToolCategoryEdit,
		fn: func(ctx context.Context, args json.RawMessage) (types.ToolResult, error) {
			if activeReads.Load() != 0 {
				t.Errorf("edit_file ran concurrently with read_file")
			}
			return types.ToolResult{Content: "edited ok"}, nil
		},
	}

	prov := &scriptedChatProvider{
		steps: []*types.ChatResponse{
			{
				ToolCalls: []types.ToolCall{
					{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"f":1}`)},
					{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"f":2}`)},
					{ID: "c3", Name: "read_file", Arguments: json.RawMessage(`{"f":3}`)},
					{ID: "c4", Name: "edit_file", Arguments: json.RawMessage(`{"f":1}`)},
				},
				InputTokens:  100,
				OutputTokens: 30,
			},
			{
				Content:      "All files read and edited.",
				InputTokens:  150,
				OutputTokens: 12,
				CachedTokens: 90,
			},
		},
	}

	ag, err := New(Options{
		Provider:     prov,
		SystemPrompt: "test system prompt",
		Tools:        []types.Tool{readTool, editTool},
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	var streamed strings.Builder
	finalText, metrics, err := ag.RunTurn(context.Background(), "inspect and fix", func(ev types.StreamEvent) {
		if ev.Type == types.EventTextDelta {
			streamed.WriteString(ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if finalText != "All files read and edited." || streamed.String() != "All files read and edited." {
		t.Fatalf("unexpected final text: %q", finalText)
	}
	if maxConcurrentReads.Load() < 2 {
		t.Fatalf("expected concurrent execution for contiguous read_file calls, got maxConcurrentReads=%d", maxConcurrentReads.Load())
	}
	if metrics.LLMCalls != 2 || metrics.ToolCalls != 4 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	if metrics.InputTokens != 250 || metrics.CachedTokens != 90 {
		t.Fatalf("unexpected token metrics: %+v", metrics)
	}

	// Ensure deterministic tool message ordering in second LLM request.
	secondReq := prov.reqs[1]
	var toolIDs []string
	for _, m := range secondReq.Messages {
		if m.Role == types.RoleTool {
			toolIDs = append(toolIDs, m.ToolCallID)
		}
	}
	if strings.Join(toolIDs, ",") != "c1,c2,c3,c4" {
		t.Fatalf("expected deterministic tool order c1,c2,c3,c4, got %v", toolIDs)
	}
}

func TestCompactPreservesUserTurnBoundariesAndToolPairs(t *testing.T) {
	prov := &scriptedChatProvider{}
	ag, err := New(Options{
		Provider:        prov,
		MaxHistoryChars: 500,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	bigToolOutput := strings.Repeat("X", 2000)
	ag.history = []types.Message{
		{Role: types.RoleUser, Content: "turn 1"},
		{Role: types.RoleAssistant, ToolCalls: []types.ToolCall{{ID: "t1", Name: "read_file"}}},
		{Role: types.RoleTool, ToolCallID: "t1", Content: bigToolOutput},
		{Role: types.RoleAssistant, Content: "done 1"},
		{Role: types.RoleUser, Content: "turn 2"},
		{Role: types.RoleAssistant, ToolCalls: []types.ToolCall{{ID: "t2", Name: "read_file"}}},
		{Role: types.RoleTool, ToolCallID: "t2", Content: "small"},
		{Role: types.RoleAssistant, Content: "done 2"},
	}

	saved := ag.Compact(1)
	if saved <= 0 {
		t.Fatalf("expected Compact to reclaim characters, got %d", saved)
	}
	hist := ag.History()
	if len(hist) == 0 || hist[0].Role != types.RoleUser || hist[0].Content != "turn 2" {
		t.Fatalf("expected compaction to slice cleanly on RoleUser boundary 'turn 2', got %+v", hist)
	}
}
