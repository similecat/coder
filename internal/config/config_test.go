package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coder/internal/types"
)

func clearCoderEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"CODER_PROVIDER",
		"CODER_MODEL",
		"CODER_APPROVAL_POLICY",
		"CODER_COPILOT_COMPAT",
		"CODER_LOAD_COPILOT_INSTRUCTIONS",
		"CODER_LOAD_COPILOT_SKILLS",
		"CODER_LOAD_AGENTS_MD",
		"CODER_BASE_URL",
		"CODER_API_KEY",
		"CODER_MAX_STEPS",
		"CODER_COMMAND_TIMEOUT_SEC",
		"CODER_STREAM",
		"OPENAI_API_KEY",
		"OPENROUTER_API_KEY",
		"ANTHROPIC_API_KEY",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestDefaultConfigAndProviders(t *testing.T) {
	clearCoderEnv(t)
	home := t.TempDir()
	ws := t.TempDir()

	cfg, err := Load(ws, home, Overrides{})
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}

	if cfg.DefaultProvider != "copilot" {
		t.Errorf("DefaultProvider = %q, want %q", cfg.DefaultProvider, "copilot")
	}
	if cfg.DefaultModel != "" {
		t.Errorf("DefaultModel = %q, want empty", cfg.DefaultModel)
	}
	if cfg.ApprovalPolicy != types.ApprovalSmartAuto {
		t.Errorf("ApprovalPolicy = %q, want %q", cfg.ApprovalPolicy, types.ApprovalSmartAuto)
	}
	if cfg.MaxSteps != 40 {
		t.Errorf("MaxSteps = %d, want 40", cfg.MaxSteps)
	}
	if cfg.MaxOutputBytes != 32768 {
		t.Errorf("MaxOutputBytes = %d, want 32768", cfg.MaxOutputBytes)
	}
	if cfg.CommandTimeoutSec != 120 {
		t.Errorf("CommandTimeoutSec = %d, want 120", cfg.CommandTimeoutSec)
	}
	if cfg.Streaming {
		t.Errorf("Streaming = true, want false")
	}
	if !cfg.Compatibility.LoadCopilotInstructions || !cfg.Compatibility.LoadCopilotSkills || !cfg.Compatibility.LoadAgentsMD {
		t.Errorf("Compatibility = %+v, want all true", cfg.Compatibility)
	}

	expectedProviders := map[string]struct {
		typ     types.ProviderType
		baseURL string
		envKey  string
		model   string
		warm    bool
	}{
		"copilot":    {types.ProviderTypeCopilotSDK, "", "", "", true},
		"ollama":     {types.ProviderTypeOpenAICompat, "http://127.0.0.1:11434/v1", "", "qwen2.5-coder:14b", false},
		"local":      {types.ProviderTypeOpenAICompat, "http://127.0.0.1:8080/v1", "", "local-model", false},
		"openai":     {types.ProviderTypeOpenAICompat, "https://api.openai.com/v1", "OPENAI_API_KEY", "gpt-4.1", false},
		"openrouter": {types.ProviderTypeOpenAICompat, "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY", "anthropic/claude-sonnet-4", false},
		"anthropic":  {types.ProviderTypeAnthropic, "https://api.anthropic.com", "ANTHROPIC_API_KEY", "claude-sonnet-4-20250514", false},
	}

	for name, exp := range expectedProviders {
		prof, err := cfg.ResolveProvider(name)
		if err != nil {
			t.Fatalf("ResolveProvider(%q) error: %v", name, err)
		}
		if prof.Name != name {
			t.Errorf("%s Name = %q, want %q", name, prof.Name, name)
		}
		if prof.Type != exp.typ {
			t.Errorf("%s Type = %q, want %q", name, prof.Type, exp.typ)
		}
		if prof.BaseURL != exp.baseURL {
			t.Errorf("%s BaseURL = %q, want %q", name, prof.BaseURL, exp.baseURL)
		}
		if prof.APIKeyEnv != exp.envKey {
			t.Errorf("%s APIKeyEnv = %q, want %q", name, prof.APIKeyEnv, exp.envKey)
		}
		if prof.DefaultModel != exp.model {
			t.Errorf("%s DefaultModel = %q, want %q", name, prof.DefaultModel, exp.model)
		}
		if exp.warm && (prof.WarmSession == nil || !*prof.WarmSession) {
			t.Errorf("%s WarmSession = %v, want true", name, prof.WarmSession)
		}
	}

	if got := cfg.EffectiveModel(""); got != "" {
		t.Errorf("EffectiveModel(\"\") = %q, want empty (auto-selected at runtime)", got)
	}
}

func TestPrecedenceMergingLayers(t *testing.T) {
	clearCoderEnv(t)
	home := t.TempDir()
	ws := t.TempDir()

	// Layer 2: User config (with UTF-8 BOM to verify Windows BOM resilience)
	userDir := filepath.Join(home, ".coder")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userJSON := "\xef\xbb\xbf" + `{
  "default_provider": "ollama",
  "default_model": "user-model",
  "approval_policy": "strict_ask",
  "max_steps": 55,
  "streaming": true,
  "compatibility": {
    "load_copilot_skills": false
  },
  "providers": {
    "ollama": {
      "default_model": "qwen2.5-coder:32b"
    },
    "vllm-lan": {
      "type": "openai_compat",
      "base_url": "http://10.0.0.5:8000/v1",
      "default_model": "deepseek-v3"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte(userJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify user layer alone first
	cfgUser, err := Load(ws, home, Overrides{})
	if err != nil {
		t.Fatalf("Load user layer failed: %v", err)
	}
	if cfgUser.DefaultProvider != "ollama" || cfgUser.DefaultModel != "user-model" {
		t.Fatalf("unexpected user config: %+v", cfgUser)
	}
	if !cfgUser.Streaming {
		t.Fatalf("expected user config streaming=true")
	}
	if !cfgUser.Compatibility.LoadCopilotInstructions || cfgUser.Compatibility.LoadCopilotSkills || !cfgUser.Compatibility.LoadAgentsMD {
		t.Fatalf("partial compatibility should preserve unspecified true defaults, got %+v", cfgUser.Compatibility)
	}
	ollamaProf, err := cfgUser.ResolveProvider("ollama")
	if err != nil {
		t.Fatal(err)
	}
	if ollamaProf.BaseURL != "http://127.0.0.1:11434/v1" || ollamaProf.DefaultModel != "qwen2.5-coder:32b" {
		t.Fatalf("ollama provider should preserve default BaseURL and update DefaultModel, got %+v", ollamaProf)
	}
	if _, err := cfgUser.ResolveProvider("copilot"); err != nil {
		t.Fatalf("unmentioned default provider copilot was wiped: %v", err)
	}
	if _, err := cfgUser.ResolveProvider("vllm-lan"); err != nil {
		t.Fatalf("custom user provider vllm-lan missing: %v", err)
	}

	// Layer 3: Workspace config overrides user config
	wsDir := filepath.Join(ws, ".coder")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wsJSON := `{
  "default_provider": "openai",
  "default_model": "ws-model",
  "command_timeout_sec": 240,
  "providers": {
    "openai": {
      "base_url": "https://ws-gateway.internal/v1"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(wsDir, "config.json"), []byte(wsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgWS, err := Load(ws, home, Overrides{})
	if err != nil {
		t.Fatalf("Load workspace layer failed: %v", err)
	}
	if cfgWS.DefaultProvider != "openai" || cfgWS.DefaultModel != "ws-model" || cfgWS.MaxSteps != 55 || cfgWS.CommandTimeoutSec != 240 {
		t.Fatalf("unexpected workspace merged config: %+v", cfgWS)
	}
	openAIProf, _ := cfgWS.ResolveProvider("openai")
	if openAIProf.BaseURL != "https://ws-gateway.internal/v1" || openAIProf.DefaultModel != "gpt-4.1" {
		t.Fatalf("unexpected merged openai profile: %+v", openAIProf)
	}

	// Layer 4: Environment variables override workspace config
	t.Setenv("CODER_PROVIDER", "local")
	t.Setenv("CODER_MODEL", "env-coder-7b")
	t.Setenv("CODER_APPROVAL_POLICY", "smart_auto")
	t.Setenv("CODER_COPILOT_COMPAT", "false")
	t.Setenv("CODER_BASE_URL", "http://127.0.0.1:9090/v1")
	t.Setenv("CODER_API_KEY", "env-secret-key")
	t.Setenv("CODER_STREAM", "false")

	cfgEnv, err := Load(ws, home, Overrides{})
	if err != nil {
		t.Fatalf("Load env layer failed: %v", err)
	}
	if cfgEnv.DefaultProvider != "local" || cfgEnv.DefaultModel != "env-coder-7b" || cfgEnv.ApprovalPolicy != types.ApprovalSmartAuto {
		t.Fatalf("env vars did not override workspace config: %+v", cfgEnv)
	}
	if cfgEnv.Streaming {
		t.Fatalf("expected CODER_STREAM=false to disable streaming")
	}
	if cfgEnv.Compatibility.LoadCopilotInstructions || cfgEnv.Compatibility.LoadCopilotSkills || !cfgEnv.Compatibility.LoadAgentsMD {
		t.Fatalf("CODER_COPILOT_COMPAT=false mismatch: %+v", cfgEnv.Compatibility)
	}
	localProf, _ := cfgEnv.ResolveProvider("")
	if localProf.BaseURL != "http://127.0.0.1:9090/v1" || localProf.APIKey != "env-secret-key" {
		t.Fatalf("CODER_BASE_URL/CODER_API_KEY not applied to active provider: %+v", localProf)
	}

	// Layer 5: Explicit CLI Overrides beat everything
	cfgCLI, err := Load(ws, home, Overrides{
		Provider:      "anthropic",
		Model:         "claude-opus-4",
		FullAuto:      true,
		CopilotCompat: Bool(true),
		BaseURL:       "https://cli-proxy.example.com",
		APIKey:        "cli-key",
		Streaming:     Bool(true),
	})
	if err != nil {
		t.Fatalf("Load CLI overrides failed: %v", err)
	}
	if cfgCLI.DefaultProvider != "anthropic" || cfgCLI.DefaultModel != "claude-opus-4" || cfgCLI.ApprovalPolicy != types.ApprovalFullAuto {
		t.Fatalf("CLI overrides not applied: %+v", cfgCLI)
	}
	if !cfgCLI.Streaming {
		t.Fatalf("expected CLI streaming override true")
	}
	if !cfgCLI.Compatibility.LoadCopilotInstructions || !cfgCLI.Compatibility.LoadCopilotSkills {
		t.Fatalf("CLI CopilotCompat=true failed: %+v", cfgCLI.Compatibility)
	}
	anthProf, _ := cfgCLI.ResolveProvider("")
	if anthProf.BaseURL != "https://cli-proxy.example.com" || anthProf.APIKey != "cli-key" {
		t.Fatalf("CLI BaseURL/APIKey override failed: %+v", anthProf)
	}
}

func TestCopilotCompatGranularEnvAndSafeFlag(t *testing.T) {
	clearCoderEnv(t)
	home := t.TempDir()
	ws := t.TempDir()

	t.Setenv("CODER_COPILOT_COMPAT", "0")
	t.Setenv("CODER_LOAD_COPILOT_SKILLS", "1")

	cfg, err := Load(ws, home, Overrides{Safe: true})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Compatibility.LoadCopilotInstructions {
		t.Errorf("LoadCopilotInstructions should be false")
	}
	if !cfg.Compatibility.LoadCopilotSkills {
		t.Errorf("LoadCopilotSkills should be true via granular env override")
	}
	if cfg.ApprovalPolicy != types.ApprovalStrictAsk {
		t.Errorf("--safe override should set strict_ask, got %q", cfg.ApprovalPolicy)
	}
}

func TestSaveUserConfigAndProviderAPIKeyEnv(t *testing.T) {
	clearCoderEnv(t)
	home := t.TempDir()
	ws := t.TempDir()

	cfg := DefaultConfig()
	cfg.DefaultProvider = "openai"
	cfg.DefaultModel = "gpt-4.1-mini"
	cfg.Compatibility.LoadCopilotInstructions = false

	t.Setenv("OPENAI_API_KEY", "sk-from-openai-env")

	// ResolveProvider reads OPENAI_API_KEY without storing secret in cfg.Providers
	prof, err := cfg.ResolveProvider("openai")
	if err != nil {
		t.Fatalf("ResolveProvider failed: %v", err)
	}
	if prof.APIKey != "sk-from-openai-env" {
		t.Fatalf("expected OPENAI_API_KEY resolution, got %q", prof.APIKey)
	}

	if err := SaveUserConfig(home, cfg); err != nil {
		t.Fatalf("SaveUserConfig failed: %v", err)
	}

	savedPath := filepath.Join(home, ".coder", "config.json")
	rawBytes, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if strings.Contains(string(rawBytes), "sk-from-openai-env") {
		t.Fatalf("SaveUserConfig leaked environment API key to disk")
	}

	reloaded, err := Load(ws, home, Overrides{})
	if err != nil {
		t.Fatalf("reload saved config failed: %v", err)
	}
	if reloaded.DefaultProvider != "openai" || reloaded.DefaultModel != "gpt-4.1-mini" || reloaded.Compatibility.LoadCopilotInstructions {
		t.Fatalf("round-trip mismatch: %+v", reloaded)
	}
}

func TestInvalidConfigValuesReturnErrors(t *testing.T) {
	clearCoderEnv(t)
	home := t.TempDir()
	ws := t.TempDir()

	t.Setenv("CODER_APPROVAL_POLICY", "yolo_invalid")
	if _, err := Load(ws, home, Overrides{}); err == nil {
		t.Fatalf("expected error on invalid CODER_APPROVAL_POLICY")
	}

	t.Setenv("CODER_APPROVAL_POLICY", "")
	t.Setenv("CODER_COPILOT_COMPAT", "maybe")
	if _, err := Load(ws, home, Overrides{}); err == nil {
		t.Fatalf("expected error on invalid CODER_COPILOT_COMPAT")
	}

	t.Setenv("CODER_COPILOT_COMPAT", "")
	t.Setenv("CODER_STREAM", "maybe")
	if _, err := Load(ws, home, Overrides{}); err == nil {
		t.Fatalf("expected error on invalid CODER_STREAM")
	}
}

func TestEmptyUserHomeResolvesViaOSUserHomeDir(t *testing.T) {
	clearCoderEnv(t)
	fakeHome := t.TempDir()
	ws := t.TempDir()
	t.Setenv("USERPROFILE", fakeHome)
	t.Setenv("HOME", fakeHome)

	custom := DefaultConfig()
	custom.DefaultProvider = "ollama"
	if err := SaveUserConfig("", custom); err != nil {
		t.Fatalf("SaveUserConfig with empty userHome failed: %v", err)
	}

	loaded, err := Load(ws, "", Overrides{})
	if err != nil {
		t.Fatalf("Load with empty userHome failed: %v", err)
	}
	if loaded.DefaultProvider != "ollama" {
		t.Fatalf("DefaultProvider = %q, want %q", loaded.DefaultProvider, "ollama")
	}
}

