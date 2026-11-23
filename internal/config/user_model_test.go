package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coder/internal/types"
)

func TestSaveUserCopilotModelPreservesUnrelatedUserSettings(t *testing.T) {
	home := t.TempDir()
	path, err := UserConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	const existing = `{"default_provider":"openai","default_model":"top-level","extension":{"large":9007199254740993},"providers":{"Copilot":{"default_model":"old","api_key":"stored","custom":true},"openai":{"default_model":"keep"}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, []byte(existing)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if saved, err := SaveUserCopilotModel(home, "new-model"); err != nil || saved != path {
		t.Fatalf("save: path=%q, err=%v", saved, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var providers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(root["providers"], &providers); err != nil {
		t.Fatal(err)
	}
	if string(root["default_provider"]) != `"openai"` || string(root["default_model"]) != `"top-level"` ||
		!strings.Contains(string(root["extension"]), "9007199254740993") ||
		len(providers) != 2 || string(providers["openai"]["default_model"]) != `"keep"` ||
		string(providers["Copilot"]["api_key"]) != `"stored"` || string(providers["Copilot"]["custom"]) != `true` ||
		string(providers["Copilot"]["default_model"]) != `"new-model"` {
		t.Fatalf("unexpected user config: %s", data)
	}
}

func TestSaveUserCopilotModelDoesNotOverwriteInvalidConfig(t *testing.T) {
	home := t.TempDir()
	path, err := UserConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{`{`, `{"providers": []}`, `{"providers": {"copilot": false}}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := SaveUserCopilotModel(home, "model"); err == nil {
			t.Fatalf("save unexpectedly succeeded for %s", content)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("invalid file changed: got %q, err=%v", got, err)
		}
	}
}

func TestSaveUserSessionConfigPreservesUnrelatedFieldsAndOmitsTransientSecrets(t *testing.T) {
	clearCoderEnv(t)
	home := t.TempDir()
	ws := t.TempDir()

	path, err := UserConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	const existing = `{
  "default_provider": "copilot",
  "default_model": "stale-global-model",
  "reasoning_effort": "high",
  "extension": {"large": 9007199254740993},
  "compatibility": {
    "load_copilot_instructions": true,
    "load_copilot_skills": true,
    "load_agents_md": true,
    "custom_compat": "preserve-me"
  },
  "providers": {
    "Copilot": {"default_model": "claude-sonnet-4.5", "api_key": "github_pat_stored", "custom": true},
    "Ollama": {"default_model": "old-ollama", "extra": 42}
  }
}`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(ws, home, Overrides{
		Provider:      "ollama",
		Model:         "qwen3-coder:30b",
		BaseURL:       "http://192.168.1.25:11434/v1",
		APIKey:        "sk-transient-cli-secret-do-not-save",
		Safe:          true,
		CopilotCompat: Bool(false),
		MaxSteps:      55,
		Streaming:     Bool(true),
	})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Simulate clearing reasoning effort in session and having both sensitive and non-sensitive headers.
	loaded.ReasoningEffort = ""
	ollamaKey, ollamaProf, ok := loaded.LookupProvider("ollama")
	if !ok {
		t.Fatal("expected ollama provider in loaded config")
	}
	ollamaProf.Headers = map[string]string{
		"Authorization": "Bearer secret-bearer-token",
		"X-Client":      "coder-dev",
	}
	loaded.Providers[ollamaKey] = ollamaProf

	snap := loaded.Snapshot("ollama", "qwen3-coder:30b")
	savedPath, err := SaveUserSessionConfig(home, snap)
	if err != nil || savedPath != path {
		t.Fatalf("SaveUserSessionConfig: path=%q, err=%v", savedPath, err)
	}

	rawSaved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rawStr := string(rawSaved)

	for _, forbidden := range []string{"sk-transient-cli-secret-do-not-save", "secret-bearer-token", "stale-global-model"} {
		if strings.Contains(rawStr, forbidden) {
			t.Fatalf("saved config unexpectedly contained %q:\n%s", forbidden, rawStr)
		}
	}
	for _, want := range []string{"9007199254740993", "preserve-me", "github_pat_stored", "coder-dev"} {
		if !strings.Contains(rawStr, want) {
			t.Fatalf("saved config lost %q:\n%s", want, rawStr)
		}
	}

	nextCfg, err := Load(ws, home, Overrides{})
	if err != nil {
		t.Fatalf("next session Load failed: %v", err)
	}
	if !strings.EqualFold(nextCfg.DefaultProvider, "ollama") {
		t.Errorf("next DefaultProvider = %q, want ollama", nextCfg.DefaultProvider)
	}
	if nextCfg.DefaultModel != "" {
		t.Errorf("next DefaultModel = %q, want empty (scoped to provider)", nextCfg.DefaultModel)
	}
	if got := nextCfg.EffectiveModel("ollama"); got != "qwen3-coder:30b" {
		t.Errorf("next EffectiveModel(ollama) = %q, want qwen3-coder:30b", got)
	}
	if got := nextCfg.EffectiveModel("copilot"); got != "claude-sonnet-4.5" {
		t.Errorf("next EffectiveModel(copilot) = %q, want claude-sonnet-4.5", got)
	}
	if nextCfg.ApprovalPolicy != types.ApprovalStrictAsk {
		t.Errorf("next ApprovalPolicy = %q, want strict_ask", nextCfg.ApprovalPolicy)
	}
	if nextCfg.MaxSteps != 55 || !nextCfg.Streaming || nextCfg.ReasoningEffort != "" {
		t.Errorf("unexpected next runtime knobs: steps=%d stream=%t effort=%q", nextCfg.MaxSteps, nextCfg.Streaming, nextCfg.ReasoningEffort)
	}
	if nextCfg.Compatibility.LoadCopilotInstructions || nextCfg.Compatibility.LoadCopilotSkills || !nextCfg.Compatibility.LoadAgentsMD {
		t.Errorf("unexpected next Compatibility: %+v", nextCfg.Compatibility)
	}
	prof, err := nextCfg.ResolveProvider("ollama")
	if err != nil {
		t.Fatalf("ResolveProvider(ollama): %v", err)
	}
	if prof.BaseURL != "http://192.168.1.25:11434/v1" || prof.Headers["X-Client"] != "coder-dev" {
		t.Errorf("unexpected saved provider profile: %+v", prof)
	}
}

func TestSaveUserSessionConfigDoesNotOverwriteInvalidConfig(t *testing.T) {
	home := t.TempDir()
	path, err := UserConfigPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := DefaultConfig().Snapshot("ollama", "qwen2.5-coder:14b")
	for _, content := range []string{`{`, `{"providers": []}`, `{"compatibility": []}`, `{"providers": {"ollama": false}}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := SaveUserSessionConfig(home, snap); err == nil {
			t.Fatalf("SaveUserSessionConfig unexpectedly succeeded for %s", content)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("invalid file changed: got %q, err=%v", got, err)
		}
	}
}
