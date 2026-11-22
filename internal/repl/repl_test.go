package repl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"coder/internal/bootstrap"
	"coder/internal/config"
	"coder/internal/provider/copilot"
	"coder/internal/types"
)

type mockREPLProvider struct {
	mu      sync.Mutex
	name    string
	defMod  string
	models  []types.ModelInfo
	reqs    []types.ChatRequest
	closed  bool
	handler func(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error)
}

func (m *mockREPLProvider) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mockprov"
}

func (m *mockREPLProvider) DefaultModel() string {
	if m.defMod != "" {
		return m.defMod
	}
	return "mock-model-v1"
}

func (m *mockREPLProvider) ListModels(context.Context) ([]types.ModelInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.models) > 0 {
		return m.models, nil
	}
	return []types.ModelInfo{
		{ID: "mock-model-v1", Provider: m.Name(), ContextSize: 128000},
		{ID: "mock-model-fast", Provider: m.Name(), ContextSize: 64000},
	}, nil
}

func (m *mockREPLProvider) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockREPLProvider) StreamChat(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	fn := m.handler
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, req, onEvent)
	}
	reply := "ack: " + req.Messages[len(req.Messages)-1].Content
	onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: reply})
	return &types.ChatResponse{Content: reply, InputTokens: 10, OutputTokens: 5}, nil
}

func (m *mockREPLProvider) Requests() []types.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]types.ChatRequest, len(m.reqs))
	copy(out, m.reqs)
	return out
}

func (m *mockREPLProvider) IsClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestREPLSlashCommandsAndLiveCompatReload(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()

	// Populate native .coder + Copilot instructions, scoped rule, skills, and custom tool.
	writeTestFile(t, filepath.Join(ws, "main.cpp"), "int main() { return 0; }\n")
	writeTestFile(t, filepath.Join(ws, ".coder", "instructions.md"), "Native coder repo guideline: prefer constexpr.")
	writeTestFile(t, filepath.Join(ws, ".github", "copilot-instructions.md"), "Copilot repo marker: FOLLOW_COPILOT_GLOBAL_RULE.")
	writeTestFile(t, filepath.Join(ws, ".github", "instructions", "cpp.instructions.md"), "---\napplyTo: \"**/*.cpp\"\n---\nScoped C++ rule: USE_SCOPED_CPP20.")
	writeTestFile(t, filepath.Join(ws, ".github", "skills", "telemetry-codec", "SKILL.md"), "---\nname: telemetry-codec\ndescription: Binary frame codec spec\n---\nFrame magic header is 0xCAFE.")
	writeTestFile(t, filepath.Join(ws, ".github", "skills", "telemetry-codec", "schema.json"), `{"magic":"0xCAFE"}`)
	writeTestFile(t, filepath.Join(ws, ".coder", "skills", "native-skill", "SKILL.md"), "---\nname: native-skill\ndescription: Native coder workflow\n---\nAlways run unit tests.")
	writeTestFile(t, filepath.Join(ws, ".coder", "tools", "lint_check.json"), `{"name":"lint_check","description":"Run custom lint check","command":"echo","args":["ok"],"read_only":true}`)

	mockProv := &mockREPLProvider{}
	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir:   ws,
		UserHome:       home,
		CustomProvider: mockProv,
		Interactive:    config.Bool(false),
	})
	if err != nil {
		t.Fatalf("bootstrap.Build failed: %v", err)
	}
	defer func() { _ = rt.Close() }()

	script := strings.Join([]string{
		"/help",
		"/skills",
		"/skill telemetry-codec",
		"/instructions",
		"/tools",
		"/mode full_auto",
		"/model",
		"/model mock-model-fast",
		"/provider",
		"first turn with copilot on",
		"/compat off",
		"/skills",
		"/instructions",
		"second turn with copilot off",
		"/compat on",
		"/exit",
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), rt, Options{
		Stdin:   strings.NewReader(script),
		Stdout:  &stdout,
		Stderr:  &stderr,
		NoColor: true,
		Version: "0.1.0-test",
	})
	if err != nil {
		t.Fatalf("repl.Run returned error: %v\nstderr:\n%s", err, stderr.String())
	}

	out := stdout.String()
	errText := stderr.String()

	// 1. Startup banner checks
	for _, want := range []string{
		"coder v0.1.0-test",
		ws,
		"provider: mockprov/mock-model-v1",
		"mode: smart_auto",
		"copilot-compat: on",
		"2 global + 1/1 scoped instructions | 2 skills | 1 custom tools",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner/stdout missing %q\nfull stdout:\n%s", want, out)
		}
	}

	// 2. Slash command output checks
	for _, want := range []string{
		"/compat [on|off]",
		"telemetry-codec [copilot_repo]",
		"native-skill [coder_repo]",
		"Companion files (1): schema.json",
		"Frame magic header is 0xCAFE.",
		"[coder_repo]",
		"[copilot_repo] applyTo=\"**/*.cpp\" (active)",
		"lint_check",
		"load_skill",
		"Approval policy set to full_auto",
		"Switched model to mock-model-fast",
		"Configured providers",
		"Copilot compatibility off: 1 global + 0/0 scoped instructions, 1 skills, 1 custom tools",
		"Copilot compatibility on: 2 global + 1/1 scoped instructions, 2 skills, 1 custom tools",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing expected output %q\nfull stdout:\n%s", want, out)
		}
	}

	if rt.Gate.Policy() != types.ApprovalFullAuto {
		t.Errorf("Gate.Policy() = %q, want %q", rt.Gate.Policy(), types.ApprovalFullAuto)
	}
	if rt.Agent.Model() != "mock-model-fast" {
		t.Errorf("Agent.Model() = %q, want %q", rt.Agent.Model(), "mock-model-fast")
	}

	// 3. Verify live prompt rebuild on /compat off -> /compat on
	reqs := mockProv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 LLM requests, got %d", len(reqs))
	}
	if !strings.Contains(reqs[0].SystemPrompt, "FOLLOW_COPILOT_GLOBAL_RULE") || !strings.Contains(reqs[0].SystemPrompt, "telemetry-codec") {
		t.Errorf("turn 1 SystemPrompt should include Copilot instructions & skill:\n%s", reqs[0].SystemPrompt)
	}
	if strings.Contains(reqs[1].SystemPrompt, "FOLLOW_COPILOT_GLOBAL_RULE") || strings.Contains(reqs[1].SystemPrompt, "telemetry-codec") {
		t.Errorf("turn 2 SystemPrompt after '/compat off' must NOT include Copilot instructions or skills:\n%s", reqs[1].SystemPrompt)
	}
	if !strings.Contains(reqs[1].SystemPrompt, "Native coder repo guideline") || !strings.Contains(reqs[1].SystemPrompt, "native-skill") {
		t.Errorf("turn 2 SystemPrompt should retain native .coder instructions & skills:\n%s", reqs[1].SystemPrompt)
	}

	// 4. Telemetry line on stderr
	if !strings.Contains(errText, "[ttft:") || !strings.Contains(errText, "provider: mockprov/mock-model-fast]") {
		t.Errorf("stderr missing expected telemetry line:\n%s", errText)
	}
}

func TestREPLMultiTurnToolStreamingCompactClearAndProviderSwitch(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()

	bigContent := strings.Repeat("line-of-source-code-0123456789abcdef\n", 40) // ~1480 bytes (> 600 compact threshold)
	writeTestFile(t, filepath.Join(ws, "sample.txt"), bigContent)

	step := 0
	mockProv := &mockREPLProvider{
		handler: func(_ context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
			step++
			switch step {
			case 1:
				// Turn 1, Step 1: invoke read_file tool
				return &types.ChatResponse{
					ToolCalls: []types.ToolCall{
						{
							ID:        "call_1",
							Name:      "read_file",
							Arguments: json.RawMessage(`{"path":"sample.txt"}`),
						},
					},
				}, nil
			case 2:
				// Turn 1, Step 2: stream final answer
				onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: "Read sample.txt successfully."})
				return &types.ChatResponse{Content: "Read sample.txt successfully."}, nil
			default:
				// Subsequent turns: echo message count in history
				msg := "history_msgs=" + string(rune('0'+len(req.Messages)))
				onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: msg})
				return &types.ChatResponse{Content: msg}, nil
			}
		},
	}

	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir:   ws,
		UserHome:       home,
		CustomProvider: mockProv,
		Interactive:    config.Bool(false),
	})
	if err != nil {
		t.Fatalf("bootstrap.Build failed: %v", err)
	}
	defer func() { _ = rt.Close() }()

	// Multi-line prompt with '\', followed by second turn, /compact 1, /clear, /provider switch, and EOF exit.
	script := strings.Join([]string{
		`please inspect \`,
		`sample.txt`,
		`follow up turn`,
		`/compact 1`,
		`/clear`,
		`/provider ollama qwen2.5-coder:7b`,
	}, "\n")

	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), rt, Options{
		Stdin:   strings.NewReader(script),
		Stdout:  &stdout,
		Stderr:  &stderr,
		NoColor: true,
	})
	if err != nil {
		t.Fatalf("repl.Run error: %v", err)
	}

	out := stdout.String()
	errText := stderr.String()

	// Verify multi-line prompt continuation was joined cleanly
	reqs := mockProv.Requests()
	if len(reqs) < 3 {
		t.Fatalf("expected at least 3 LLM calls across 2 turns, got %d", len(reqs))
	}
	if reqs[0].Messages[0].Content != "please inspect \nsample.txt" {
		t.Errorf("multi-line prompt = %q, want %q", reqs[0].Messages[0].Content, "please inspect \nsample.txt")
	}

	// Verify Turn 2 saw Turn 1's conversation history (4 messages: user1, assistant_toolcall, tool_result, assistant1 + user2 = 5)
	if len(reqs[2].Messages) != 5 {
		t.Errorf("turn 2 request message count = %d, want 5", len(reqs[2].Messages))
	}

	// Verify tool progress indicators on stderr
	if !strings.Contains(errText, `-> read_file {"path":"sample.txt"}`) {
		t.Errorf("stderr missing tool start indicator:\n%s", errText)
	}
	if !strings.Contains(errText, `<- read_file ok (`) {
		t.Errorf("stderr missing tool end indicator:\n%s", errText)
	}
	if !strings.Contains(errText, "llm: 2 | tools: 1") {
		t.Errorf("stderr missing accurate llm/tool counts:\n%s", errText)
	}

	// Verify /compact 1 reclaimed characters from Turn 1's large tool result
	if strings.Contains(out, "reclaimed 0 characters") || !strings.Contains(out, "Compacted history: reclaimed ") {
		t.Errorf("expected /compact 1 to reclaim >0 characters, stdout:\n%s", out)
	}

	// Verify /clear emptied history and /provider switched live provider to ollama
	if !strings.Contains(out, "Conversation cleared.") {
		t.Errorf("stdout missing 'Conversation cleared.':\n%s", out)
	}
	if len(rt.Agent.History()) != 0 {
		t.Errorf("expected empty history after /clear, got %d messages", len(rt.Agent.History()))
	}
	if !strings.Contains(out, "Switched provider to ollama (model: qwen2.5-coder:7b)") {
		t.Errorf("stdout missing provider switch confirmation:\n%s", out)
	}
	if rt.Agent.ProviderName() != "ollama" || rt.Agent.Model() != "qwen2.5-coder:7b" {
		t.Errorf("active provider/model = %s/%s, want ollama/qwen2.5-coder:7b", rt.Agent.ProviderName(), rt.Agent.Model())
	}
}

func TestREPLCtrlCCancelsActiveTurnWhileKeepingSessionAlive(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()

	interruptCh := make(chan os.Signal, 1)
	turnCount := 0

	mockProv := &mockREPLProvider{
		handler: func(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
			turnCount++
			if turnCount == 1 {
				onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: "working..."})
				// Trigger simulated Ctrl+C while Turn 1 is mid-flight!
				interruptCh <- os.Interrupt
				<-ctx.Done()
				return nil, ctx.Err()
			}
			onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: "recovered cleanly"})
			return &types.ChatResponse{Content: "recovered cleanly"}, nil
		},
	}

	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir:   ws,
		UserHome:       home,
		CustomProvider: mockProv,
		Interactive:    config.Bool(false),
	})
	if err != nil {
		t.Fatalf("bootstrap.Build failed: %v", err)
	}
	defer func() { _ = rt.Close() }()

	script := "slow turn\nfast turn\n/quit\n"
	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), rt, Options{
		Stdin:       strings.NewReader(script),
		Stdout:      &stdout,
		Stderr:      &stderr,
		NoColor:     true,
		InterruptCh: interruptCh,
	})
	if err != nil {
		t.Fatalf("repl.Run failed: %v", err)
	}

	if mockProv.IsClosed() {
		t.Errorf("provider must remain warm/open during REPL Ctrl+C turn cancellation")
	}
	if !strings.Contains(stderr.String(), "[interrupted]") {
		t.Errorf("expected [interrupted] on stderr, got:\n%s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovered cleanly") {
		t.Errorf("expected second turn after Ctrl+C to succeed, stdout:\n%s", stdout.String())
	}
}

func TestREPLInteractiveAuthRecoveryAndCancel(t *testing.T) {
	ws := t.TempDir()
	home := t.TempDir()

	authFailProv := &mockREPLProvider{
		name: "copilot",
		handler: func(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
			return nil, copilot.ErrNotAuthenticated
		},
	}

	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir:   ws,
		UserHome:       home,
		CustomProvider: authFailProv,
		Interactive:    config.Bool(false),
	})
	if err != nil {
		t.Fatalf("bootstrap.Build failed: %v", err)
	}
	defer func() { _ = rt.Close() }()

	allowPrompt := true
	var stdout, stderr bytes.Buffer
	err = Run(context.Background(), rt, Options{
		Stdin:           strings.NewReader("hello\nn\n/quit\n"),
		Stdout:          &stdout,
		Stderr:          &stderr,
		NoColor:         true,
		AllowAuthPrompt: &allowPrompt,
	})
	if err != nil {
		t.Fatalf("repl.Run failed: %v", err)
	}

	if !strings.Contains(stderr.String(), "GitHub Copilot CLI is not signed in yet.") ||
		!strings.Contains(stderr.String(), "[1] Sign in now with GitHub OAuth") {
		t.Fatalf("expected interactive Copilot auth recovery menu on stderr, got:\n%s", stderr.String())
	}
}

