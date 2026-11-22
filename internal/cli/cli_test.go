package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"coder/internal/config"
	"coder/internal/types"
)

type mockCLIProvider struct {
	mu      sync.Mutex
	name    string
	model   string
	reqs    []types.ChatRequest
	handler func(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error)
}

func (m *mockCLIProvider) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mockcli"
}

func (m *mockCLIProvider) DefaultModel() string {
	if m.model != "" {
		return m.model
	}
	return "mock-cli-v1"
}

func (m *mockCLIProvider) ListModels(context.Context) ([]types.ModelInfo, error) {
	return []types.ModelInfo{{ID: m.DefaultModel(), Provider: m.Name()}}, nil
}

func (m *mockCLIProvider) Close() error { return nil }

func (m *mockCLIProvider) StreamChat(ctx context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	fn := m.handler
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, req, onEvent)
	}
	text := "response to: " + req.Messages[len(req.Messages)-1].Content
	onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: text})
	return &types.ChatResponse{
		Content:      text,
		InputTokens:  12,
		OutputTokens: 8,
	}, nil
}

func clearTestEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CODER_PROVIDER", "CODER_MODEL", "CODER_APPROVAL_POLICY",
		"CODER_COPILOT_COMPAT", "CODER_BASE_URL", "CODER_API_KEY", "CODER_STREAM",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

func TestCLIVersionAndHelp(t *testing.T) {
	clearTestEnv(t)

	for _, flag := range [][]string{{"--version"}, {"-v"}, {"version"}} {
		var stdout, stderr bytes.Buffer
		code := Execute(flag, strings.NewReader(""), &stdout, &stderr, nil)
		if code != 0 {
			t.Fatalf("Execute(%v) = %d, want 0 (stderr: %s)", flag, code, stderr.String())
		}
		if strings.TrimSpace(stdout.String()) != "coder v"+Version {
			t.Errorf("Execute(%v) stdout = %q, want %q", flag, stdout.String(), "coder v"+Version)
		}
	}

	var helpOut, helpErr bytes.Buffer
	code := Execute([]string{"--help"}, strings.NewReader(""), &helpOut, &helpErr, nil)
	if code != 0 {
		t.Fatalf("Execute(--help) = %d, want 0", code)
	}
	for _, sub := range []string{"coder exec", "coder repl", "coder doctor", "coder config", "coder bench"} {
		if !strings.Contains(helpOut.String(), sub) {
			t.Errorf("help output missing %q", sub)
		}
	}
}

func TestCLIExecStreamingJSONStdinAndPermissions(t *testing.T) {
	clearTestEnv(t)
	ws := t.TempDir()
	home := t.TempDir()

	// 1. Plain streaming mode with flags both before and after subcommand
	prov := &mockCLIProvider{}
	var stdout, stderr bytes.Buffer
	code := ExecuteWithOptions(RunOptions{
		Args:           []string{"-C", ws, "exec", "explain", "architecture", "-m", "custom-m1", "--stream=false", "--no-color"},
		Stdin:          strings.NewReader(""),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: prov,
		UserHome:       home,
	})
	if code != 0 {
		t.Fatalf("exec plain returned %d, stderr: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "response to: explain architecture" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "response to: explain architecture\n")
	}
	if !strings.Contains(stderr.String(), "provider: mockcli/custom-m1]") {
		t.Errorf("stderr missing telemetry summary: %s", stderr.String())
	}

	// 2. Piped stdin with --quiet
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"exec", "--quiet", "-C", ws},
		Stdin:          strings.NewReader("prompt from pipe\n"),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: prov,
		UserHome:       home,
	})
	if code != 0 {
		t.Fatalf("exec piped stdin returned %d, stderr: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "response to: prompt from pipe" {
		t.Errorf("piped stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr with --quiet, got %q", stderr.String())
	}

	// 3. --json output on success (no "error" key emitted)
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"run", "--json", "-C", ws, "-q", "compute checksum"},
		Stdin:          strings.NewReader(""),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: prov,
		UserHome:       home,
	})
	if code != 0 {
		t.Fatalf("run --json returned %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), `"error"`) {
		t.Errorf("successful --json output should omit error field, got:\n%s", stdout.String())
	}
	var parsedJSON ExecJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &parsedJSON); err != nil {
		t.Fatalf("unmarshal --json output: %v\nraw:\n%s", err, stdout.String())
	}
	if parsedJSON.Output != "response to: compute checksum" || parsedJSON.Provider != "mockcli" || parsedJSON.Metrics.LLMCalls != 1 {
		t.Errorf("unexpected ExecJSONOutput: %+v", parsedJSON)
	}

	// 3b. Shorthand -e, '-' stdin sentinel, default positional prompt, and default no-args REPL entry
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"-C", ws, "-e", "-q", "short flag prompt"},
		Stdin:          strings.NewReader(""),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: prov,
		UserHome:       home,
	})
	if code != 0 || strings.TrimSpace(stdout.String()) != "response to: short flag prompt" {
		t.Fatalf("-e shorthand failed (code=%d): %q / %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"-C", ws, "-q", "direct positional prompt"},
		Stdin:          strings.NewReader(""),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: prov,
		UserHome:       home,
	})
	if code != 0 || strings.TrimSpace(stdout.String()) != "response to: direct positional prompt" {
		t.Fatalf("direct positional prompt failed (code=%d): %q", code, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"-C", ws, "--no-color"},
		Stdin:          strings.NewReader("/help\n/exit\n"),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: prov,
		UserHome:       home,
	})
	if code != 0 || !strings.Contains(stdout.String(), "coder v"+Version) || !strings.Contains(stdout.String(), "/compat") {
		t.Fatalf("default no-args REPL entry failed (code=%d): %q / %q", code, stdout.String(), stderr.String())
	}

	// 4. --json output on provider error
	errProv := &mockCLIProvider{
		handler: func(context.Context, types.ChatRequest, func(types.StreamEvent)) (*types.ChatResponse, error) {
			return nil, errors.New("upstream rate limit")
		},
	}
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"exec", "--json", "-C", ws, "trigger error"},
		Stdin:          strings.NewReader(""),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: errProv,
		UserHome:       home,
	})
	if code != 1 {
		t.Fatalf("expected exit code 1 on error, got %d", code)
	}
	var errJSON ExecJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &errJSON); err != nil {
		t.Fatalf("unmarshal error JSON: %v", err)
	}
	if !strings.Contains(errJSON.Error, "upstream rate limit") {
		t.Errorf("expected error message in JSON, got %+v", errJSON)
	}

	// 5. Non-interactive smart_auto vs --full-auto shell command behavior
	makeShellProvider := func() *mockCLIProvider {
		called := false
		return &mockCLIProvider{
			handler: func(_ context.Context, req types.ChatRequest, onEvent func(types.StreamEvent)) (*types.ChatResponse, error) {
				if !called {
					called = true
					return &types.ChatResponse{
						ToolCalls: []types.ToolCall{
							{
								ID:        "cmd_1",
								Name:      "run_command",
								Arguments: json.RawMessage(`{"command":"echo FULL_AUTO_OK"}`),
							},
						},
					}, nil
				}
				lastToolMsg := req.Messages[len(req.Messages)-1]
				onEvent(types.StreamEvent{Type: types.EventTextDelta, Text: lastToolMsg.Content})
				return &types.ChatResponse{Content: lastToolMsg.Content}, nil
			},
		}
	}

	// Without --full-auto (non-interactive): run_command must be denied under smart_auto
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"exec", "-C", ws, "--no-color", "try shell"},
		Stdin:          strings.NewReader(""),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: makeShellProvider(),
		UserHome:       home,
	})
	if code != 0 {
		t.Fatalf("exec smart_auto returned %d", code)
	}
	if !strings.Contains(stdout.String(), "denied") {
		t.Errorf("expected run_command denial in non-interactive smart_auto, got: %s", stdout.String())
	}

	// With --full-auto: run_command succeeds!
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:           []string{"exec", "-C", ws, "--full-auto", "--no-color", "try shell"},
		Stdin:          strings.NewReader(""),
		Stdout:         &stdout,
		Stderr:         &stderr,
		CustomProvider: makeShellProvider(),
		UserHome:       home,
	})
	if code != 0 {
		t.Fatalf("exec --full-auto returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "FULL_AUTO_OK") {
		t.Errorf("expected FULL_AUTO_OK with --full-auto, got: %s", stdout.String())
	}
}

func TestCLIConfigInitShowAndAPIKeyRedaction(t *testing.T) {
	clearTestEnv(t)
	ws := t.TempDir()
	home := t.TempDir()

	var stdout, stderr bytes.Buffer

	// 1. config path
	code := ExecuteWithOptions(RunOptions{
		Args:     []string{"config", "path", "-C", ws},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code != 0 {
		t.Fatalf("config path returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), filepath.Join(ws, ".coder", "config.json")) ||
		!strings.Contains(stdout.String(), filepath.Join(home, ".coder", "config.json")) {
		t.Errorf("unexpected config path output:\n%s", stdout.String())
	}

	// 2. config init (workspace) + duplicate refusal + --force overwrite
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:     []string{"config", "init", "-C", ws, "-p", "ollama"},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code != 0 {
		t.Fatalf("config init failed (%d): %s", code, stderr.String())
	}
	wsCfgFile := filepath.Join(ws, ".coder", "config.json")
	if _, err := os.Stat(wsCfgFile); err != nil {
		t.Fatalf("expected %s to exist: %v", wsCfgFile, err)
	}

	// Second init without --force must fail
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:     []string{"config", "init", "-C", ws},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code == 0 {
		t.Fatalf("expected config init without --force to fail when file already exists")
	}

	// With --force succeeds
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:     []string{"config", "init", "-C", ws, "--force"},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code != 0 {
		t.Fatalf("config init --force failed (%d): %s", code, stderr.String())
	}

	// 3. config init --user
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:     []string{"config", "init", "--user"},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code != 0 {
		t.Fatalf("config init --user failed (%d): %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".coder", "config.json")); err != nil {
		t.Fatalf("expected user config file to exist: %v", err)
	}

	// 4. Write sensitive API key into workspace config AND pass --api-key flag; verify redaction in `config show`
	rawSecretJSON := `{
  "default_provider": "openai",
  "providers": {
    "openai": {
      "type": "openai_compat",
      "base_url": "https://api.openai.com/v1",
      "api_key": "sk-top-secret-file-key-12345",
      "headers": {
        "Authorization": "Bearer secret-header-token-67890",
        "X-Custom": "keep-visible"
      }
    },
    "anthropic": {
      "type": "anthropic",
      "api_key": "sk-ant-secret-abcdef"
    }
  }
}`
	if err := os.WriteFile(wsCfgFile, []byte(rawSecretJSON), 0o644); err != nil {
		t.Fatalf("write secret config: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:     []string{"config", "show", "-C", ws, "--api-key", "sk-cli-override-99999"},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code != 0 {
		t.Fatalf("config show failed (%d): %s", code, stderr.String())
	}

	shown := stdout.String()
	for _, secret := range []string{"sk-top-secret-file-key-12345", "secret-header-token-67890", "sk-ant-secret-abcdef", "sk-cli-override-99999"} {
		if strings.Contains(shown, secret) {
			t.Fatalf("SECURITY LEAK: config show leaked secret %q:\n%s", secret, shown)
		}
	}
	if !strings.Contains(shown, "***REDACTED***") {
		t.Errorf("expected ***REDACTED*** marker in config show:\n%s", shown)
	}
	if !strings.Contains(shown, "keep-visible") {
		t.Errorf("expected non-sensitive header to remain visible:\n%s", shown)
	}

	var decoded config.Config
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("config show did not emit valid JSON: %v", err)
	}
}

func TestCLIDoctorAndBenchDelegation(t *testing.T) {
	clearTestEnv(t)
	ws := t.TempDir()
	home := t.TempDir()

	// Create sample Copilot customization in workspace for doctor discovery
	if err := os.MkdirAll(filepath.Join(ws, ".github", "skills", "doc-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".github", "copilot-instructions.md"), []byte("Be concise."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".github", "skills", "doc-skill", "SKILL.md"), []byte("---\nname: doc-skill\ndescription: demo\n---\nBody"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mock local Ollama server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	origProbe := LocalProbeURL
	LocalProbeURL = srv.URL + "/api/tags"
	t.Cleanup(func() {
		LocalProbeURL = origProbe
	})

	var stdout, stderr bytes.Buffer
	code := ExecuteWithOptions(RunOptions{
		Args:     []string{"doctor", "-C", ws},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code != 0 {
		t.Fatalf("coder doctor failed (%d): %s", code, stderr.String())
	}
	docOut := stdout.String()
	for _, want := range []string{
		"coder doctor",
		"1 global instructions",
		"1 skills",
		"doc-skill",
		"local-llm:",
		"reachable at",
		"ripgrep:",
	} {
		if !strings.Contains(docOut, want) {
			t.Errorf("doctor output missing %q:\n%s", want, docOut)
		}
	}

	// Close local server and verify doctor remains non-fatal (exit 0) when offline, and respects --no-copilot-compat
	srv.Close()
	stdout.Reset()
	stderr.Reset()
	code = ExecuteWithOptions(RunOptions{
		Args:     []string{"doctor", "-C", ws, "--no-copilot-compat", "--json"},
		Stdout:   &stdout,
		Stderr:   &stderr,
		UserHome: home,
	})
	if code != 0 {
		t.Fatalf("coder doctor offline failed (%d): %s", code, stderr.String())
	}
	var docReport DoctorReport
	if err := json.Unmarshal(stdout.Bytes(), &docReport); err != nil {
		t.Fatalf("unmarshal doctor --json: %v", err)
	}
	if docReport.Skills != 0 || docReport.GlobalInstructions != 0 {
		t.Errorf("expected 0 Copilot skills/instructions with --no-copilot-compat, got %+v", docReport)
	}

	// Test `coder bench` delegation
	var gotBenchArgs []string
	benchFn := func(args []string, out, _ io.Writer) int {
		gotBenchArgs = append([]string(nil), args...)
		_, _ = io.WriteString(out, "bench scorecard ok\n")
		return 0
	}

	stdout.Reset()
	stderr.Reset()
	code = Execute(
		[]string{"bench", "--provider", "copilot", "--problems", "cpp_lru_ttl", "--json"},
		strings.NewReader(""),
		&stdout,
		&stderr,
		benchFn,
	)
	if code != 0 {
		t.Fatalf("coder bench returned %d", code)
	}
	wantArgs := []string{"--provider", "copilot", "--problems", "cpp_lru_ttl", "--json"}
	if strings.Join(gotBenchArgs, "|") != strings.Join(wantArgs, "|") {
		t.Errorf("benchFn received %v, want %v", gotBenchArgs, wantArgs)
	}
	if !strings.Contains(stdout.String(), "bench scorecard ok") {
		t.Errorf("bench stdout = %q", stdout.String())
	}
}
