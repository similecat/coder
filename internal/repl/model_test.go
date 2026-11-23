package repl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"coder/internal/bootstrap"
	"coder/internal/config"
	"coder/internal/types"
)

func TestModelSavePersistsOnlyExplicitSelection(t *testing.T) {
	t.Setenv("CODER_MODEL", "")
	t.Setenv("CODER_PROVIDER", "")
	ws, home := t.TempDir(), t.TempDir()
	mock := &mockREPLProvider{name: "copilot", models: []types.ModelInfo{{ID: "gpt-6.2-sol"}, {ID: "auto"}}}
	build := func(overrides config.Overrides) *bootstrap.Runtime {
		t.Helper()
		rt, err := bootstrap.Build(bootstrap.Options{WorkspaceDir: ws, UserHome: home, CustomProvider: mock, Overrides: overrides})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rt.Close() })
		return rt
	}
	path, err := config.UserConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	rt := build(config.Overrides{Model: "temporary-cli-model"})
	s := &sessionState{rt: rt, stdout: &out, stderr: &stderr}
	run := func(command string) error {
		t.Helper()
		_, err := s.handleSlashCommand(context.Background(), command)
		return err
	}
	if err := run("/model list"); err != nil || !strings.Contains(out.String(), "gpt-6.2-sol") {
		t.Fatalf("list: %v, %s", err, &out)
	}
	if err := run("/model auto"); err != nil || rt.Agent.Model() != "auto" {
		t.Fatalf("temporary switch: %v, model=%s", err, rt.Agent.Model())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session-only switch wrote config or stat failed: %v", err)
	}
	if err := run("/model GPT-6.2-SOL --save"); err != nil || rt.Agent.Model() != "gpt-6.2-sol" {
		t.Fatalf("save: %v, model=%s", err, rt.Agent.Model())
	}
	if !strings.Contains(out.String(), "Saved Copilot default gpt-6.2-sol") || !strings.Contains(stderr.String(), "temporary-cli-model") {
		t.Fatalf("expected save confirmation and override warning; got %s / %s", &out, &stderr)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/model missing --save", "/model list --save", "/model auto --typo"} {
		if err := run(command); err == nil || rt.Agent.Model() != "gpt-6.2-sol" {
			t.Fatalf("expected %q to fail without switching; got %v, model=%s", command, err, rt.Agent.Model())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("invalid commands changed user config: %v", err)
	}
	if restarted := build(config.Overrides{}); restarted.Agent.Model() != "gpt-6.2-sol" {
		t.Fatalf("new session model: %s", restarted.Agent.Model())
	}
}

func TestConfigSavePersistsFullSessionStateForNextSession(t *testing.T) {
	t.Setenv("CODER_MODEL", "")
	t.Setenv("CODER_PROVIDER", "")
	t.Setenv("CODER_APPROVAL_POLICY", "")
	t.Setenv("CODER_COPILOT_COMPAT", "")
	t.Setenv("CODER_STREAM", "")
	t.Setenv("CODER_API_KEY", "")

	ws, home := t.TempDir(), t.TempDir()

	// Session 1: started with CLI overrides, then further modified via REPL slash commands.
	rt1, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir: ws,
		UserHome:     home,
		Overrides: config.Overrides{
			Provider:        "ollama",
			Model:           "initial-cli-model",
			BaseURL:         "http://127.0.0.1:11499/v1",
			APIKey:          "sk-ephemeral-flag-secret",
			MaxSteps:        64,
			ReasoningEffort: "medium",
			Streaming:       config.Bool(true),
		},
	})
	if err != nil {
		t.Fatalf("bootstrap session 1: %v", err)
	}
	t.Cleanup(func() { _ = rt1.Close() })

	var out, stderr bytes.Buffer
	s1 := &sessionState{rt: rt1, stdout: &out, stderr: &stderr}
	run := func(cmd string) {
		t.Helper()
		if _, err := s1.handleSlashCommand(context.Background(), cmd); err != nil {
			t.Fatalf("%s failed: %v", cmd, err)
		}
	}

	run("/model qwen2.5-coder:32b")
	run("/mode strict_ask")
	run("/compat off")

	// Simulate user answering 'a' (always) on a one-off tool prompt during the turn:
	// Gate.Policy() becomes full_auto temporarily, but /config save must NOT persist full_auto!
	rt1.Gate.SetPolicy(types.ApprovalFullAuto)

	// Verify /config show redacts secrets and reflects live model/provider.
	run("/config show")
	if strings.Contains(out.String(), "sk-ephemeral-flag-secret") {
		t.Fatalf("/config show leaked secret:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "qwen2.5-coder:32b") {
		t.Fatalf("/config show missing live model:\n%s", out.String())
	}

	out.Reset()
	stderr.Reset()
	run("/config save")

	if !strings.Contains(out.String(), "Saved session configuration") {
		t.Fatalf("missing save confirmation: %s", out.String())
	}
	if !strings.Contains(stderr.String(), "temporary prompt approval") {
		t.Fatalf("expected note about temporary prompt approval on stderr, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "transient API key") {
		t.Fatalf("expected note about omitted transient API key on stderr, got: %s", stderr.String())
	}

	// Session 2: start with ZERO overrides (simulating next `coder` launch) and verify every setting matches!
	rt2, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir: ws,
		UserHome:     home,
	})
	if err != nil {
		t.Fatalf("bootstrap session 2: %v", err)
	}
	t.Cleanup(func() { _ = rt2.Close() })

	if rt2.Agent.ProviderName() != "ollama" {
		t.Errorf("session 2 provider = %q, want ollama", rt2.Agent.ProviderName())
	}
	if rt2.Agent.Model() != "qwen2.5-coder:32b" {
		t.Errorf("session 2 model = %q, want qwen2.5-coder:32b", rt2.Agent.Model())
	}
	if rt2.Gate.Policy() != types.ApprovalStrictAsk {
		t.Errorf("session 2 policy = %q, want strict_ask", rt2.Gate.Policy())
	}
	if rt2.Config.Compatibility.LoadCopilotInstructions || rt2.Config.Compatibility.LoadCopilotSkills {
		t.Errorf("session 2 compatibility = %+v, want copilot compat off", rt2.Config.Compatibility)
	}
	if !rt2.Config.Streaming || rt2.Config.MaxSteps != 64 || rt2.Config.ReasoningEffort != "medium" {
		t.Errorf("session 2 knobs mismatch: stream=%t steps=%d effort=%q", rt2.Config.Streaming, rt2.Config.MaxSteps, rt2.Config.ReasoningEffort)
	}
	prof2, err := rt2.Config.ResolveProvider("ollama")
	if err != nil || prof2.BaseURL != "http://127.0.0.1:11499/v1" {
		t.Errorf("session 2 ollama profile = %+v, err=%v", prof2, err)
	}
	if prof2.APIKey != "" {
		t.Errorf("session 2 unexpectedly persisted transient APIKey: %q", prof2.APIKey)
	}
}

func TestSlashMutatorSaveFlagsAndWorkspaceShadowWarning(t *testing.T) {
	t.Setenv("CODER_MODEL", "")
	t.Setenv("CODER_PROVIDER", "")
	t.Setenv("CODER_APPROVAL_POLICY", "")

	ws, home := t.TempDir(), t.TempDir()

	// Write a workspace config that pins approval_policy to strict_ask so we can verify shadowing warning.
	writeTestFile(t, config.WorkspaceConfigPath(ws), `{"approval_policy": "strict_ask"}`)

	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir: ws,
		UserHome:     home,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	var out, stderr bytes.Buffer
	s := &sessionState{rt: rt, stdout: &out, stderr: &stderr}

	// Switch provider + model with --save
	if _, err := s.handleSlashCommand(context.Background(), "/provider local custom-local-v2 --save"); err != nil {
		t.Fatalf("/provider --save failed: %v", err)
	}
	// Switch mode with --save (shadowed by workspace strict_ask!)
	stderr.Reset()
	if _, err := s.handleSlashCommand(context.Background(), "/mode full_auto --save"); err != nil {
		t.Fatalf("/mode --save failed: %v", err)
	}
	if !strings.Contains(stderr.String(), "approval_policy=strict_ask") {
		t.Fatalf("expected workspace shadowing warning for approval_policy, got: %s", stderr.String())
	}

	// Non-Copilot /model <id> --save works too!
	if _, err := s.handleSlashCommand(context.Background(), "/model custom-local-v3 --save"); err != nil {
		t.Fatalf("non-copilot /model --save failed: %v", err)
	}

	// Verify in a fresh workspace (without the workspace override) that all saved settings take effect!
	freshWS := t.TempDir()
	rtNext, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir: freshWS,
		UserHome:     home,
	})
	if err != nil {
		t.Fatalf("bootstrap fresh workspace: %v", err)
	}
	t.Cleanup(func() { _ = rtNext.Close() })

	if rtNext.Agent.ProviderName() != "local" || rtNext.Agent.Model() != "custom-local-v3" || rtNext.Gate.Policy() != types.ApprovalFullAuto {
		t.Fatalf("unexpected next session state: prov=%s model=%s policy=%s", rtNext.Agent.ProviderName(), rtNext.Agent.Model(), rtNext.Gate.Policy())
	}
}

type fakeManagedAutoSession struct {
	active string
}

func (f *fakeManagedAutoSession) ActiveModel() string                  { return f.active }
func (f *fakeManagedAutoSession) SetModel(_ context.Context, m string) error { f.active = m; return nil }
func (f *fakeManagedAutoSession) SetTools(context.Context, []types.Tool) error { return nil }
func (f *fakeManagedAutoSession) Close() error                         { return nil }
func (f *fakeManagedAutoSession) RunTurn(_ context.Context, _ string, onEvent func(types.StreamEvent)) (string, types.TurnMetrics, error) {
	onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: "ok"})
	return "ok", types.TurnMetrics{Provider: "copilot", Model: f.active, LLMCalls: 1}, nil
}

type fakeManagedAutoProvider struct{}

func (fakeManagedAutoProvider) Name() string         { return "copilot" }
func (fakeManagedAutoProvider) DefaultModel() string { return "" }
func (fakeManagedAutoProvider) ListModels(context.Context) ([]types.ModelInfo, error) {
	return []types.ModelInfo{{ID: "claude-sonnet-4.5"}, {ID: "auto"}}, nil
}
func (fakeManagedAutoProvider) Close() error { return nil }
func (fakeManagedAutoProvider) CreateSession(context.Context, types.ManagedSessionConfig) (types.ManagedSession, error) {
	return &fakeManagedAutoSession{active: "claude-sonnet-4.5"}, nil
}

func TestConfigSaveKeepsCopilotAutoWhenModelWasDynamicallyResolved(t *testing.T) {
	t.Setenv("CODER_MODEL", "")
	t.Setenv("CODER_PROVIDER", "")
	ws, home := t.TempDir(), t.TempDir()

	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir:   ws,
		UserHome:       home,
		CustomProvider: fakeManagedAutoProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// Run a turn so Copilot Auto resolves runtime Agent.Model() to "claude-sonnet-4.5" while ConfiguredModel() stays "".
	if _, _, err := rt.Agent.RunTurn(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	if rt.Agent.Model() != "claude-sonnet-4.5" || rt.Agent.ConfiguredModel() != "" {
		t.Fatalf("expected active=claude-sonnet-4.5 and configured=\"\", got active=%q configured=%q", rt.Agent.Model(), rt.Agent.ConfiguredModel())
	}

	var out, stderr bytes.Buffer
	s := &sessionState{rt: rt, stdout: &out, stderr: &stderr}
	if _, err := s.handleSlashCommand(context.Background(), "/mode full_auto --save"); err != nil {
		t.Fatal(err)
	}

	nextCfg, err := config.Load(ws, home, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got := nextCfg.EffectiveModel("copilot"); got != "" {
		t.Fatalf("expected Copilot default_model to stay auto (\"\"), got %q", got)
	}
	if nextCfg.ApprovalPolicy != types.ApprovalFullAuto {
		t.Fatalf("expected saved policy full_auto, got %q", nextCfg.ApprovalPolicy)
	}
}
