package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/github/copilot-sdk/go"

	"coder/internal/types"
)

type stubTool struct {
	def    types.ToolDefinition
	execFn func(ctx context.Context, args json.RawMessage) (types.ToolResult, error)
}

func (s *stubTool) Definition() types.ToolDefinition {
	return s.def
}

func (s *stubTool) Execute(ctx context.Context, args json.RawMessage) (types.ToolResult, error) {
	if s.execFn != nil {
		return s.execFn(ctx, args)
	}
	return types.ToolResult{Content: "ok"}, nil
}

func TestNormalizeToolArguments(t *testing.T) {
	t.Run("nil arguments normalize to empty JSON object", func(t *testing.T) {
		got, err := NormalizeToolArguments(nil)
		if err != nil || string(got) != "{}" {
			t.Fatalf("got=%s err=%v", string(got), err)
		}
	})

	t.Run("JSON string is not double-quoted", func(t *testing.T) {
		input := `{"path":"internal/main.go","line":12}`
		got, err := NormalizeToolArguments(input)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if string(got) != input {
			t.Fatalf("expected unquoted JSON object %s, got %s", input, string(got))
		}
		var parsed map[string]any
		if err := json.Unmarshal(got, &parsed); err != nil {
			t.Fatalf("expected JSON object unmarshal to succeed: %v", err)
		}
		if parsed["path"] != "internal/main.go" {
			t.Fatalf("unexpected path: %v", parsed["path"])
		}
	})

	t.Run("plain non-JSON string is JSON-encoded", func(t *testing.T) {
		got, err := NormalizeToolArguments("hello world")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if string(got) != `"hello world"` {
			t.Fatalf("expected quoted JSON string, got %s", string(got))
		}
	})

	t.Run("json.RawMessage preserved cleanly", func(t *testing.T) {
		raw := json.RawMessage(` {"a": 1} `)
		got, err := NormalizeToolArguments(raw)
		if err != nil || string(got) != `{"a": 1}` {
			t.Fatalf("got=%s err=%v", string(got), err)
		}
	})

	t.Run("map[string]any marshaled cleanly", func(t *testing.T) {
		got, err := NormalizeToolArguments(map[string]any{"cmd": "go test ./..."})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if string(got) != `{"cmd":"go test ./..."}` {
			t.Fatalf("unexpected marshaled map: %s", string(got))
		}
	})
}

func TestBuildSessionConfig_LowLatencyFlags(t *testing.T) {
	readTool := &stubTool{
		def: types.ToolDefinition{
			Name:        "view_file",
			Description: "Read file contents",
			Parameters:  map[string]any{"type": "object"},
			Category:    types.ToolCategoryRead,
		},
	}
	editTool := &stubTool{
		def: types.ToolDefinition{
			Name:        "edit_file",
			Description: "Edit file contents",
			Parameters:  map[string]any{"type": "object"},
			Category:    types.ToolCategoryEdit,
		},
	}

	cfg := BuildSessionConfig(types.ManagedSessionConfig{
		Model:           "",
		SystemPrompt:    "You are coder.",
		WorkingDir:      `D:\work\coder`,
		Tools:           []types.Tool{readTool, editTool},
		ReasoningEffort: "high",
		Streaming:       true,
	}, "claude-sonnet-4.5", nil)

	if cfg.ClientName != "coder" {
		t.Fatalf("expected ClientName=coder, got %q", cfg.ClientName)
	}
	if cfg.Model != "claude-sonnet-4.5" {
		t.Fatalf("expected fallback defaultModel claude-sonnet-4.5, got %q", cfg.Model)
	}
	if cfg.ReasoningEffort != "high" {
		t.Fatalf("expected ReasoningEffort=high, got %q", cfg.ReasoningEffort)
	}
	if cfg.Streaming == nil || !*cfg.Streaming {
		t.Fatal("expected Streaming=true")
	}
	if cfg.AllowMultipleToolCalls == nil || !*cfg.AllowMultipleToolCalls {
		t.Fatal("expected AllowMultipleToolCalls=true")
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(cfg.Tools))
	}
	for _, st := range cfg.Tools {
		if !st.OverridesBuiltInTool {
			t.Fatalf("expected tool %q OverridesBuiltInTool=true", st.Name)
		}
		if !st.SkipPermission {
			t.Fatalf("expected tool %q SkipPermission=true", st.Name)
		}
	}
	if len(cfg.AvailableTools) != 2 || cfg.AvailableTools[0] != "view_file" || cfg.AvailableTools[1] != "edit_file" {
		t.Fatalf("unexpected AvailableTools: %+v", cfg.AvailableTools)
	}
	if cfg.IncludedBuiltInAgents == nil || len(cfg.IncludedBuiltInAgents) != 0 {
		t.Fatalf("expected empty non-nil IncludedBuiltInAgents, got %#v", cfg.IncludedBuiltInAgents)
	}
	if cfg.EnableConfigDiscovery == nil || *cfg.EnableConfigDiscovery {
		t.Fatal("expected EnableConfigDiscovery=false")
	}
	if cfg.SkipCustomInstructions == nil || !*cfg.SkipCustomInstructions {
		t.Fatal("expected SkipCustomInstructions=true")
	}
	if cfg.EnableSkills == nil || *cfg.EnableSkills {
		t.Fatal("expected EnableSkills=false")
	}
	if cfg.SkipEmbeddingRetrieval == nil || !*cfg.SkipEmbeddingRetrieval {
		t.Fatal("expected SkipEmbeddingRetrieval=true")
	}
	if cfg.EnableFileHooks == nil || *cfg.EnableFileHooks {
		t.Fatal("expected EnableFileHooks=false")
	}
	if cfg.EnableSessionStore == nil || *cfg.EnableSessionStore {
		t.Fatal("expected EnableSessionStore=false")
	}
	if cfg.OnPermissionRequest == nil {
		t.Fatal("expected OnPermissionRequest to be set to ApproveAll")
	}
	if cfg.WorkingDirectory != `D:\work\coder` {
		t.Fatalf("unexpected WorkingDirectory: %q", cfg.WorkingDirectory)
	}
	if cfg.SystemMessage == nil || cfg.SystemMessage.Mode != "replace" || cfg.SystemMessage.Content != "You are coder." {
		t.Fatalf("unexpected SystemMessage: %+v", cfg.SystemMessage)
	}
}

func TestBuildSessionConfig_DefaultStreamingDisabled(t *testing.T) {
	cfg := BuildSessionConfig(types.ManagedSessionConfig{}, "auto", nil)
	if cfg.Streaming == nil || *cfg.Streaming {
		t.Fatalf("expected Streaming=false by default, got %v", cfg.Streaming)
	}
}

func TestConvertTools_HandlerTelemetryAndSelfCorrectingErrors(t *testing.T) {
	var events []string
	hook := &ToolInvokeHook{
		OnStart: func(id, name, args string) {
			events = append(events, "start:"+id+":"+name+":"+args)
		},
		OnFinish: func(id, name, args string, res types.ToolResult) {
			events = append(events, "end:"+id+":"+res.Content)
		},
	}

	okTool := &stubTool{
		def: types.ToolDefinition{Name: "shell"},
		execFn: func(ctx context.Context, args json.RawMessage) (types.ToolResult, error) {
			return types.ToolResult{Content: "PASS: 5 tests"}, nil
		},
	}
	failTool := &stubTool{
		def: types.ToolDefinition{Name: "edit"},
		execFn: func(ctx context.Context, args json.RawMessage) (types.ToolResult, error) {
			return types.ToolResult{Content: "old_str not found in file.go", IsError: true}, errors.New("edit failed")
		},
	}

	sdkTools, _ := ConvertTools([]types.Tool{okTool, failTool}, hook)

	res1, err1 := sdkTools[0].Handler(sdk.ToolInvocation{
		ToolCallID: "call_1",
		ToolName:   "shell",
		Arguments:  `{"cmd":"go test"}`,
	})
	if err1 != nil {
		t.Fatalf("expected nil Go error, got %v", err1)
	}
	if res1.ResultType != "success" || res1.TextResultForLLM != "PASS: 5 tests" {
		t.Fatalf("unexpected success ToolResult: %+v", res1)
	}

	res2, err2 := sdkTools[1].Handler(sdk.ToolInvocation{
		ToolCallID: "call_2",
		ToolName:   "edit",
		Arguments:  map[string]any{"path": "file.go"},
	})
	if err2 != nil {
		t.Fatalf("expected nil Go error on tool failure so LLM receives error message, got %v", err2)
	}
	if res2.ResultType != "failure" || res2.TextResultForLLM != "old_str not found in file.go" || res2.Error != "old_str not found in file.go" {
		t.Fatalf("unexpected failure ToolResult: %+v", res2)
	}

	if len(events) != 4 {
		t.Fatalf("expected 4 hook events, got %v", events)
	}
}

type fakeSDKSession struct {
	mu           sync.Mutex
	handlers     []sdk.SessionEventHandler
	cfg          *sdk.SessionConfig
	sendFn       func(ctx context.Context, s *fakeSDKSession, opts sdk.MessageOptions) (*sdk.SessionEvent, error)
	aborted      atomic.Bool
	disconnected atomic.Bool
	lastModel    string
	updatedTools []sdk.Tool
}

func (f *fakeSDKSession) On(h sdk.SessionEventHandler) func() {
	f.mu.Lock()
	idx := len(f.handlers)
	f.handlers = append(f.handlers, h)
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.handlers[idx] = nil
		f.mu.Unlock()
	}
}

func (f *fakeSDKSession) emit(ev sdk.SessionEvent) {
	f.mu.Lock()
	hs := append([]sdk.SessionEventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range hs {
		if h != nil {
			h(ev)
		}
	}
}

func (f *fakeSDKSession) Send(ctx context.Context, opts sdk.MessageOptions) (string, error) {
	go func() {
		resp, err := f.sendFn(ctx, f, opts)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				f.emit(sdk.SessionEvent{Data: &sdk.SessionErrorData{Message: err.Error()}})
			}
			return
		}
		if resp != nil {
			f.emit(*resp)
		}
		f.emit(sdk.SessionEvent{Data: &sdk.SessionIdleData{}})
	}()
	return "msg_1", nil
}

func (f *fakeSDKSession) SendAndWait(ctx context.Context, opts sdk.MessageOptions) (*sdk.SessionEvent, error) {
	return f.sendFn(ctx, f, opts)
}

func (f *fakeSDKSession) Abort(ctx context.Context) error {
	f.aborted.Store(true)
	return nil
}

func (f *fakeSDKSession) SetModel(ctx context.Context, model string, opts *sdk.SetModelOptions) error {
	f.mu.Lock()
	f.lastModel = model
	f.mu.Unlock()
	return nil
}

func (f *fakeSDKSession) SetTools(ctx context.Context, tools []sdk.Tool) error {
	f.mu.Lock()
	f.updatedTools = tools
	f.mu.Unlock()
	return nil
}

func (f *fakeSDKSession) Disconnect() error {
	f.disconnected.Store(true)
	return nil
}

type fakeSDKClient struct {
	starts     atomic.Int32
	stops      atomic.Int32
	authStatus *sdk.GetAuthStatusResponse
	sessHook   func(cfg *sdk.SessionConfig) *fakeSDKSession
	catalog    []sdk.ModelInfo
	createErr  func(model string) error
}

func (c *fakeSDKClient) GetAuthStatus(ctx context.Context) (*sdk.GetAuthStatusResponse, error) {
	if c.authStatus != nil {
		return c.authStatus, nil
	}
	return &sdk.GetAuthStatusResponse{IsAuthenticated: true}, nil
}

func (c *fakeSDKClient) Start(ctx context.Context) error {
	c.starts.Add(1)
	return nil
}

func (c *fakeSDKClient) Stop() error {
	c.stops.Add(1)
	return nil
}

func (c *fakeSDKClient) ForceStop() {
	c.stops.Add(1)
}

func (c *fakeSDKClient) CreateSession(ctx context.Context, cfg *sdk.SessionConfig) (sdkSession, error) {
	if c.createErr != nil {
		if err := c.createErr(cfg.Model); err != nil {
			return nil, err
		}
	}
	return c.sessHook(cfg), nil
}

func (c *fakeSDKClient) ListModels(ctx context.Context) ([]sdk.ModelInfo, error) {
	if c.catalog != nil {
		return c.catalog, nil
	}
	ctxWin := 200000
	return []sdk.ModelInfo{{
		ID:   "claude-sonnet-4.5",
		Name: "Claude Sonnet 4.5",
		Capabilities: sdk.ModelCapabilities{
			Limits: sdk.ModelLimits{MaxContextWindowTokens: &ctxWin},
		},
	}}, nil
}

func TestProviderWarmClientAndRunTurnAndAbort(t *testing.T) {
	var createdOpts *sdk.ClientOptions
	var lastFakeSess *fakeSDKSession

	fc := &fakeSDKClient{
		sessHook: func(cfg *sdk.SessionConfig) *fakeSDKSession {
			s := &fakeSDKSession{cfg: cfg}
			lastFakeSess = s
			return s
		},
	}

	p, err := New(types.ProviderProfile{
		Name:         "copilot",
		Type:         types.ProviderTypeCopilotSDK,
		APIKey:       "ghu_test_token",
		DefaultModel: "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("unexpected New error: %v", err)
	}
	p.clientFactory = func(opts *sdk.ClientOptions) sdkClient {
		createdOpts = opts
		return fc
	}

	// Client must NOT start eagerly in New()
	if fc.starts.Load() != 0 {
		t.Fatal("expected lazy client start, got eager start")
	}

	models, err := p.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ContextSize != 200000 {
		t.Fatalf("unexpected ListModels: %+v err=%v", models, err)
	}
	if fc.starts.Load() != 1 {
		t.Fatalf("expected 1 client start, got %d", fc.starts.Load())
	}
	if createdOpts.LogLevel != "error" || createdOpts.GitHubToken != "ghu_test_token" {
		t.Fatalf("unexpected ClientOptions: %+v", createdOpts)
	}

	tool := &stubTool{
		def: types.ToolDefinition{Name: "read_file"},
		execFn: func(ctx context.Context, args json.RawMessage) (types.ToolResult, error) {
			return types.ToolResult{Content: "package main"}, nil
		},
	}

	ms, err := p.CreateSession(context.Background(), types.ManagedSessionConfig{
		SystemPrompt: "Be concise.",
		Tools:        []types.Tool{tool},
	})
	if err != nil {
		t.Fatalf("unexpected CreateSession error: %v", err)
	}
	// Warm client reused!
	if fc.starts.Load() != 1 {
		t.Fatalf("expected warm client reuse (starts=1), got %d", fc.starts.Load())
	}

	// Turn 1: streaming reasoning + tool invocation + text deltas + usage
	lastFakeSess.sendFn = func(ctx context.Context, s *fakeSDKSession, opts sdk.MessageOptions) (*sdk.SessionEvent, error) {
		time.Sleep(2 * time.Millisecond)
		s.emit(sdk.SessionEvent{Data: &sdk.AssistantTurnStartData{TurnID: "t1"}})
		s.emit(sdk.SessionEvent{Data: &sdk.AssistantReasoningDeltaData{DeltaContent: "Thinking..."}})
		_, _ = s.cfg.Tools[0].Handler(sdk.ToolInvocation{
			ToolCallID: "tc_1",
			ToolName:   "read_file",
			Arguments:  `{"path":"main.go"}`,
		})
		s.emit(sdk.SessionEvent{Data: &sdk.AssistantTurnStartData{TurnID: "t2"}})
		s.emit(sdk.SessionEvent{Data: &sdk.AssistantMessageDeltaData{DeltaContent: "Done "}})
		s.emit(sdk.SessionEvent{Data: &sdk.AssistantMessageDeltaData{DeltaContent: "now!"}})
		inTok, outTok, cacheTok := int64(500), int64(40), int64(300)
		s.emit(sdk.SessionEvent{Data: &sdk.AssistantUsageData{
			InputTokens:     &inTok,
			OutputTokens:    &outTok,
			CacheReadTokens: &cacheTok,
		}})
		return &sdk.SessionEvent{Data: &sdk.AssistantMessageData{Content: "Done now!"}}, nil
	}

	var streamedEvents []types.StreamEvent
	finalText, metrics, err := ms.RunTurn(context.Background(), "Inspect main.go", func(ev types.StreamEvent) {
		streamedEvents = append(streamedEvents, ev)
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if finalText != "Done now!" {
		t.Fatalf("expected 'Done now!', got %q", finalText)
	}
	if metrics.LLMCalls != 2 || metrics.ToolCalls != 1 || metrics.InputTokens != 500 || metrics.OutputTokens != 40 || metrics.CachedTokens != 300 || metrics.TTFT <= 0 {
		t.Fatalf("unexpected TurnMetrics: %+v", metrics)
	}
	if len(streamedEvents) != 6 {
		t.Fatalf("expected 6 stream events (reasoning, tool_start, tool_end, 2 text, usage), got %d: %+v", len(streamedEvents), streamedEvents)
	}

	// Turn 2: non-delta fallback extracts AssistantMessageData from SendAndWait response
	lastFakeSess.sendFn = func(ctx context.Context, s *fakeSDKSession, opts sdk.MessageOptions) (*sdk.SessionEvent, error) {
		return &sdk.SessionEvent{Data: &sdk.AssistantMessageData{Content: "Non-streamed reply"}}, nil
	}
	nonDeltaText, nonDeltaMetrics, err := ms.RunTurn(context.Background(), "Quick ping", nil)
	if err != nil || nonDeltaText != "Non-streamed reply" || nonDeltaMetrics.LLMCalls != 1 {
		t.Fatalf("expected non-delta fallback 'Non-streamed reply', got %q metrics=%+v err=%v", nonDeltaText, nonDeltaMetrics, err)
	}

	// Turn 3: mid-turn context cancellation triggers session.Abort without stopping warm client
	cancelCtx, cancel := context.WithCancel(context.Background())
	lastFakeSess.sendFn = func(ctx context.Context, s *fakeSDKSession, opts sdk.MessageOptions) (*sdk.SessionEvent, error) {
		cancel()
		deadline := time.Now().Add(2 * time.Second)
		for !s.aborted.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		return nil, ctx.Err()
	}
	_, _, err = ms.RunTurn(cancelCtx, "Long task", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !lastFakeSess.aborted.Load() {
		t.Fatal("expected session.Abort to be called on context cancellation")
	}
	if fc.stops.Load() != 0 {
		t.Fatal("warm client must NOT stop when a single turn is cancelled")
	}

	// SetModel & SetTools & Session.Close
	if err := ms.SetModel(context.Background(), "claude-opus-4.6"); err != nil || lastFakeSess.lastModel != "claude-opus-4.6" {
		t.Fatalf("SetModel failed: err=%v model=%q", err, lastFakeSess.lastModel)
	}
	if err := ms.SetTools(context.Background(), []types.Tool{tool}); err != nil || len(lastFakeSess.updatedTools) != 1 {
		t.Fatalf("SetTools failed: err=%v", err)
	}
	if err := ms.Close(); err != nil || !lastFakeSess.disconnected.Load() {
		t.Fatalf("Session.Close failed: err=%v", err)
	}
	if fc.stops.Load() != 0 {
		t.Fatal("Session.Close must keep Provider client warm")
	}

	// Provider.Close stops the warm client
	if err := p.Close(); err != nil || fc.stops.Load() != 1 {
		t.Fatalf("Provider.Close expected stops=1, got stops=%d err=%v", fc.stops.Load(), err)
	}
}

func TestCreateSession_DetectsUnauthenticatedRuntime(t *testing.T) {
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_PROVIDER_BASE_URL"} {
		t.Setenv(k, "")
	}

	fc := &fakeSDKClient{
		authStatus: &sdk.GetAuthStatusResponse{IsAuthenticated: false},
	}
	p, err := New(types.ProviderProfile{
		Name:         "copilot",
		Type:         types.ProviderTypeCopilotSDK,
		DefaultModel: "claude-sonnet-4.5",
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	p.clientFactory = func(opts *sdk.ClientOptions) sdkClient {
		return fc
	}

	_, err = p.CreateSession(context.Background(), types.ManagedSessionConfig{
		SystemPrompt: "test",
	})
	if err == nil {
		t.Fatal("expected ErrNotAuthenticated when IsAuthenticated=false, got nil")
	}
	if !IsAuthError(err) || !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("expected IsAuthError/ErrNotAuthenticated, got %v", err)
	}
}

func TestSelectDefaultModelAndFallbackWhenUnavailable(t *testing.T) {
	catalog := []sdk.ModelInfo{
		{ID: "auto", Name: "Auto"},
		{ID: "disabled-codex", Name: "Disabled Codex", Policy: &sdk.ModelPolicy{State: "disabled"}},
		{ID: "gemini-3.8-flash", Name: "Gemini Flash"},
		{ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex"},
		{ID: "gpt-6.2-sol", Name: "GPT-6.2 Sol"},
	}

	picked := SelectDefaultModel(catalog)
	if picked != "gpt-5.3-codex" {
		t.Fatalf("expected gpt-5.3-codex (skipping disabled-codex and auto), got %q", picked)
	}

	chosen, fellBack := ResolveAvailableModel("claude-sonnet-4.5", catalog)
	if !fellBack || chosen != "auto" {
		t.Fatalf("expected fallback to auto, got chosen=%q fellBack=%v", chosen, fellBack)
	}
	catalog[0].Policy = &sdk.ModelPolicy{State: "disabled"}
	chosen, fellBack = ResolveAvailableModel("claude-sonnet-4.5", catalog)
	if !fellBack || chosen != "gpt-5.3-codex" {
		t.Fatalf("expected coding model when Auto is disabled, got chosen=%q fellBack=%v", chosen, fellBack)
	}

	// Explicit available model ("gpt-6.2-sol") is preserved
	chosen2, fellBack2 := ResolveAvailableModel("gpt-6.2-sol", catalog)
	if fellBack2 || chosen2 != "gpt-6.2-sol" {
		t.Fatalf("expected exact gpt-6.2-sol without fallback, got chosen=%q fellBack=%v", chosen2, fellBack2)
	}
}
