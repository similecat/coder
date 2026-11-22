package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"coder/internal/types"
)

const (
	configDirName  = ".coder"
	configFileName = "config.json"
)

// Compatibility controls loading of GitHub Copilot and AGENTS.md ecosystems alongside .coder.
type Compatibility struct {
	LoadCopilotInstructions bool `json:"load_copilot_instructions"`
	LoadCopilotSkills       bool `json:"load_copilot_skills"`
	LoadAgentsMD            bool `json:"load_agents_md"`
}

// DefaultCompatibility returns the default compatibility configuration (all enabled).
func DefaultCompatibility() Compatibility {
	return Compatibility{
		LoadCopilotInstructions: true,
		LoadCopilotSkills:       true,
		LoadAgentsMD:            true,
	}
}

// Config holds the resolved runtime configuration for coder.
type Config struct {
	DefaultProvider   string                           `json:"default_provider"`
	DefaultModel      string                           `json:"default_model,omitempty"`
	ApprovalPolicy    types.ApprovalPolicy             `json:"approval_policy"`
	MaxSteps          int                              `json:"max_steps"`
	MaxOutputBytes    int                              `json:"max_output_bytes"`
	CommandTimeoutSec int                              `json:"command_timeout_sec"`
	ReasoningEffort   string                           `json:"reasoning_effort,omitempty"`
	Streaming         bool                             `json:"streaming"`
	Compatibility     Compatibility                    `json:"compatibility"`
	Providers         map[string]types.ProviderProfile `json:"providers"`
}

// Overrides captures highest-precedence explicit CLI flag overrides.
type Overrides struct {
	Provider                string
	Model                   string
	ApprovalPolicy          types.ApprovalPolicy
	FullAuto                bool
	Safe                    bool
	CopilotCompat           *bool
	LoadCopilotInstructions *bool
	LoadCopilotSkills       *bool
	LoadAgentsMD            *bool
	BaseURL                 string
	APIKey                  string
	MaxSteps                int
	MaxOutputBytes          int
	CommandTimeoutSec       int
	ReasoningEffort         string
	Streaming               *bool
}

// Bool returns a pointer to v for populating optional boolean fields.
func Bool(v bool) *bool {
	return &v
}

// DefaultProviders returns the built-in provider profiles map.
func DefaultProviders() map[string]types.ProviderProfile {
	return map[string]types.ProviderProfile{
		"copilot": {
			Name:         "copilot",
			Type:         types.ProviderTypeCopilotSDK,
			DefaultModel: "",
			WarmSession:  Bool(true),
		},
		"ollama": {
			Name:         "ollama",
			Type:         types.ProviderTypeOpenAICompat,
			BaseURL:      "http://127.0.0.1:11434/v1",
			DefaultModel: "qwen2.5-coder:14b",
		},
		"lmstudio": {
			Name:         "lmstudio",
			Type:         types.ProviderTypeOpenAICompat,
			BaseURL:      "http://127.0.0.1:1234/v1",
			DefaultModel: "",
		},
		"local": {
			Name:         "local",
			Type:         types.ProviderTypeOpenAICompat,
			BaseURL:      "http://127.0.0.1:8080/v1",
			DefaultModel: "local-model",
		},
		"openai": {
			Name:         "openai",
			Type:         types.ProviderTypeOpenAICompat,
			BaseURL:      "https://api.openai.com/v1",
			APIKeyEnv:    "OPENAI_API_KEY",
			DefaultModel: "gpt-4.1",
		},
		"openrouter": {
			Name:         "openrouter",
			Type:         types.ProviderTypeOpenAICompat,
			BaseURL:      "https://openrouter.ai/api/v1",
			APIKeyEnv:    "OPENROUTER_API_KEY",
			DefaultModel: "anthropic/claude-sonnet-4",
		},
		"anthropic": {
			Name:         "anthropic",
			Type:         types.ProviderTypeAnthropic,
			BaseURL:      "https://api.anthropic.com",
			APIKeyEnv:    "ANTHROPIC_API_KEY",
			DefaultModel: "claude-sonnet-4-20250514",
		},
	}
}

// DefaultConfig returns a fresh Config initialized with built-in defaults.
func DefaultConfig() *Config {
	return &Config{
		DefaultProvider:   "copilot",
		DefaultModel:      "",
		ApprovalPolicy:    types.ApprovalSmartAuto,
		MaxSteps:          40,
		MaxOutputBytes:    32768,
		CommandTimeoutSec: 120,
		Streaming:         false,
		Compatibility:     DefaultCompatibility(),
		Providers:         DefaultProviders(),
	}
}

// UserConfigPath resolves <userHome>\.coder\config.json, falling back to os.UserHomeDir when userHome is empty.
func UserConfigPath(userHome string) (string, error) {
	home := strings.TrimSpace(userHome)
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home dir: %w", err)
		}
	}
	return filepath.Join(home, configDirName, configFileName), nil
}

// WorkspaceConfigPath resolves <workspaceDir>\.coder\config.json.
func WorkspaceConfigPath(workspaceDir string) string {
	ws := strings.TrimSpace(workspaceDir)
	if ws == "" {
		return ""
	}
	return filepath.Join(ws, configDirName, configFileName)
}

// Load resolves configuration in precedence order:
// 1. Built-in defaults
// 2. User config (<userHome>\.coder\config.json)
// 3. Workspace config (<workspaceDir>\.coder\config.json)
// 4. Environment variables
// 5. Explicit CLI Overrides
func Load(workspaceDir string, userHome string, overrides Overrides) (*Config, error) {
	cfg := DefaultConfig()

	home := strings.TrimSpace(userHome)
	if home == "" {
		if resolved, err := os.UserHomeDir(); err == nil {
			home = resolved
		}
	}

	var userCfgPath string
	if home != "" {
		userCfgPath = filepath.Join(home, configDirName, configFileName)
		if err := mergeConfigFile(cfg, userCfgPath); err != nil {
			return nil, err
		}
	}

	if wsCfgPath := WorkspaceConfigPath(workspaceDir); wsCfgPath != "" {
		if userCfgPath == "" || !samePath(userCfgPath, wsCfgPath) {
			if err := mergeConfigFile(cfg, wsCfgPath); err != nil {
				return nil, err
			}
		}
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}

	if err := applyOverrides(cfg, overrides); err != nil {
		return nil, err
	}

	// Apply CODER_BASE_URL / CODER_API_KEY and Overrides.BaseURL / Overrides.APIKey
	// to the final active DefaultProvider profile in precedence order (env -> overrides).
	envBaseURL := strings.TrimSpace(os.Getenv("CODER_BASE_URL"))
	envAPIKey := strings.TrimSpace(os.Getenv("CODER_API_KEY"))
	overrideBaseURL := strings.TrimSpace(overrides.BaseURL)
	overrideAPIKey := strings.TrimSpace(overrides.APIKey)

	effectiveBaseURL := envBaseURL
	if overrideBaseURL != "" {
		effectiveBaseURL = overrideBaseURL
	}
	effectiveAPIKey := envAPIKey
	if overrideAPIKey != "" {
		effectiveAPIKey = overrideAPIKey
	}

	if (effectiveBaseURL != "" || effectiveAPIKey != "") && cfg.DefaultProvider != "" {
		key, prof, found := lookupProvider(cfg.Providers, cfg.DefaultProvider)
		if !found {
			key = cfg.DefaultProvider
			prof = types.ProviderProfile{
				Name: key,
				Type: types.ProviderTypeOpenAICompat,
			}
		}
		if effectiveBaseURL != "" {
			prof.BaseURL = effectiveBaseURL
		}
		if effectiveAPIKey != "" {
			prof.APIKey = effectiveAPIKey
		}
		cfg.Providers[key] = prof
	}

	return cfg, nil
}

// SaveUserConfig writes cfg atomically to <userHome>\.coder\config.json.
func SaveUserConfig(userHome string, cfg *Config) error {
	if cfg == nil {
		return errors.New("cannot save nil config")
	}
	cfgPath, err := UserConfigPath(userHome)
	if err != nil {
		return err
	}
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create user config dir %s: %w", dir, err)
	}

	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	encoded = append(encoded, '\n')

	tmpFile, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(encoded); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp config file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync temp config file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp config file: %w", err)
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		return fmt.Errorf("replace config file %s: %w", cfgPath, err)
	}
	cleanup = false
	return nil
}

// SaveUserConfig writes the receiver Config to <userHome>\.coder\config.json.
func (c *Config) SaveUserConfig(userHome string) error {
	return SaveUserConfig(userHome, c)
}

// ResolveProvider looks up a provider profile by name (or DefaultProvider if empty),
// returning a cloned profile with APIKey resolved from APIKeyEnv or CODER_API_KEY when needed.
func (c *Config) ResolveProvider(name string) (types.ProviderProfile, error) {
	if c == nil {
		return types.ProviderProfile{}, errors.New("nil config")
	}
	target := strings.TrimSpace(name)
	if target == "" {
		target = strings.TrimSpace(c.DefaultProvider)
	}
	if target == "" {
		return types.ProviderProfile{}, errors.New("no provider specified")
	}

	key, prof, ok := lookupProvider(c.Providers, target)
	if !ok {
		return types.ProviderProfile{}, fmt.Errorf("unknown provider %q", target)
	}

	cloned := cloneProviderProfile(prof)
	if cloned.Name == "" {
		cloned.Name = key
	}
	if cloned.APIKey == "" && cloned.APIKeyEnv != "" {
		if envVal := strings.TrimSpace(os.Getenv(cloned.APIKeyEnv)); envVal != "" {
			cloned.APIKey = envVal
		}
	}
	if cloned.APIKey == "" {
		if envVal := strings.TrimSpace(os.Getenv("CODER_API_KEY")); envVal != "" {
			cloned.APIKey = envVal
		}
	}
	return cloned, nil
}

// EffectiveModel returns DefaultModel if explicitly configured, or the provider's DefaultModel otherwise.
func (c *Config) EffectiveModel(providerName string) string {
	if c == nil {
		return ""
	}
	if m := strings.TrimSpace(c.DefaultModel); m != "" {
		return m
	}
	prof, err := c.ResolveProvider(providerName)
	if err != nil {
		return ""
	}
	return prof.DefaultModel
}

type partialCompatibility struct {
	LoadCopilotInstructions *bool `json:"load_copilot_instructions"`
	LoadCopilotSkills       *bool `json:"load_copilot_skills"`
	LoadAgentsMD            *bool `json:"load_agents_md"`
}

type partialProviderProfile struct {
	Name         *string             `json:"name"`
	Type         *types.ProviderType `json:"type"`
	BaseURL      *string             `json:"base_url"`
	APIKey       *string             `json:"api_key"`
	APIKeyEnv    *string             `json:"api_key_env"`
	DefaultModel *string             `json:"default_model"`
	APIVersion   *string             `json:"api_version"`
	Headers      map[string]string   `json:"headers"`
	TimeoutSec   *int                `json:"timeout_sec"`
	WarmSession  *bool               `json:"warm_session"`
}

type partialConfig struct {
	DefaultProvider   *string                           `json:"default_provider"`
	DefaultModel      *string                           `json:"default_model"`
	ApprovalPolicy    *string                           `json:"approval_policy"`
	MaxSteps          *int                              `json:"max_steps"`
	MaxOutputBytes    *int                              `json:"max_output_bytes"`
	CommandTimeoutSec *int                              `json:"command_timeout_sec"`
	ReasoningEffort   *string                           `json:"reasoning_effort"`
	Streaming         *bool                             `json:"streaming"`
	Compatibility     *partialCompatibility             `json:"compatibility"`
	Providers         map[string]partialProviderProfile `json:"providers"`
}

func mergeConfigFile(cfg *Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var p partialConfig
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}

	if p.DefaultProvider != nil {
		if v := strings.TrimSpace(*p.DefaultProvider); v != "" {
			cfg.DefaultProvider = v
		}
	}
	if p.DefaultModel != nil {
		cfg.DefaultModel = strings.TrimSpace(*p.DefaultModel)
	}
	if p.ApprovalPolicy != nil && strings.TrimSpace(*p.ApprovalPolicy) != "" {
		pol, err := parseApprovalPolicy(*p.ApprovalPolicy)
		if err != nil {
			return fmt.Errorf("config %s: %w", path, err)
		}
		cfg.ApprovalPolicy = pol
	}
	if p.MaxSteps != nil && *p.MaxSteps > 0 {
		cfg.MaxSteps = *p.MaxSteps
	}
	if p.MaxOutputBytes != nil && *p.MaxOutputBytes > 0 {
		cfg.MaxOutputBytes = *p.MaxOutputBytes
	}
	if p.CommandTimeoutSec != nil && *p.CommandTimeoutSec > 0 {
		cfg.CommandTimeoutSec = *p.CommandTimeoutSec
	}
	if p.ReasoningEffort != nil {
		cfg.ReasoningEffort = strings.TrimSpace(*p.ReasoningEffort)
	}
	if p.Streaming != nil {
		cfg.Streaming = *p.Streaming
	}
	if p.Compatibility != nil {
		if p.Compatibility.LoadCopilotInstructions != nil {
			cfg.Compatibility.LoadCopilotInstructions = *p.Compatibility.LoadCopilotInstructions
		}
		if p.Compatibility.LoadCopilotSkills != nil {
			cfg.Compatibility.LoadCopilotSkills = *p.Compatibility.LoadCopilotSkills
		}
		if p.Compatibility.LoadAgentsMD != nil {
			cfg.Compatibility.LoadAgentsMD = *p.Compatibility.LoadAgentsMD
		}
	}
	if len(p.Providers) > 0 {
		if cfg.Providers == nil {
			cfg.Providers = make(map[string]types.ProviderProfile, len(p.Providers))
		}
		for name, patch := range p.Providers {
			key := strings.TrimSpace(name)
			if key == "" {
				continue
			}
			existingKey, base, found := lookupProvider(cfg.Providers, key)
			if found {
				key = existingKey
				base = cloneProviderProfile(base)
			}
			merged := mergeProviderProfile(key, base, patch)
			cfg.Providers[key] = merged
		}
	}
	return nil
}

func mergeProviderProfile(key string, base types.ProviderProfile, patch partialProviderProfile) types.ProviderProfile {
	out := base
	if patch.Name != nil && strings.TrimSpace(*patch.Name) != "" {
		out.Name = strings.TrimSpace(*patch.Name)
	}
	if out.Name == "" {
		out.Name = key
	}
	if patch.Type != nil && strings.TrimSpace(string(*patch.Type)) != "" {
		out.Type = *patch.Type
	}
	if out.Type == "" {
		out.Type = types.ProviderTypeOpenAICompat
	}
	if patch.BaseURL != nil {
		out.BaseURL = strings.TrimSpace(*patch.BaseURL)
	}
	if patch.APIKey != nil {
		out.APIKey = strings.TrimSpace(*patch.APIKey)
	}
	if patch.APIKeyEnv != nil {
		out.APIKeyEnv = strings.TrimSpace(*patch.APIKeyEnv)
	}
	if patch.DefaultModel != nil {
		out.DefaultModel = strings.TrimSpace(*patch.DefaultModel)
	}
	if patch.APIVersion != nil {
		out.APIVersion = strings.TrimSpace(*patch.APIVersion)
	}
	if patch.Headers != nil {
		if out.Headers == nil {
			out.Headers = make(map[string]string, len(patch.Headers))
		}
		for hk, hv := range patch.Headers {
			out.Headers[hk] = hv
		}
	}
	if patch.TimeoutSec != nil && *patch.TimeoutSec >= 0 {
		out.TimeoutSec = *patch.TimeoutSec
	}
	if patch.WarmSession != nil {
		out.WarmSession = Bool(*patch.WarmSession)
	}
	return out
}

func applyEnv(cfg *Config) error {
	if v := strings.TrimSpace(os.Getenv("CODER_PROVIDER")); v != "" {
		cfg.DefaultProvider = v
	}
	if v := strings.TrimSpace(os.Getenv("CODER_MODEL")); v != "" {
		cfg.DefaultModel = v
	}
	if v := strings.TrimSpace(os.Getenv("CODER_APPROVAL_POLICY")); v != "" {
		pol, err := parseApprovalPolicy(v)
		if err != nil {
			return fmt.Errorf("CODER_APPROVAL_POLICY: %w", err)
		}
		cfg.ApprovalPolicy = pol
	}
	if v := strings.TrimSpace(os.Getenv("CODER_COPILOT_COMPAT")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("CODER_COPILOT_COMPAT: %w", err)
		}
		cfg.Compatibility.LoadCopilotInstructions = b
		cfg.Compatibility.LoadCopilotSkills = b
	}
	if v := strings.TrimSpace(os.Getenv("CODER_LOAD_COPILOT_INSTRUCTIONS")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("CODER_LOAD_COPILOT_INSTRUCTIONS: %w", err)
		}
		cfg.Compatibility.LoadCopilotInstructions = b
	}
	if v := strings.TrimSpace(os.Getenv("CODER_LOAD_COPILOT_SKILLS")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("CODER_LOAD_COPILOT_SKILLS: %w", err)
		}
		cfg.Compatibility.LoadCopilotSkills = b
	}
	if v := strings.TrimSpace(os.Getenv("CODER_LOAD_AGENTS_MD")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("CODER_LOAD_AGENTS_MD: %w", err)
		}
		cfg.Compatibility.LoadAgentsMD = b
	}
	if v := strings.TrimSpace(os.Getenv("CODER_MAX_STEPS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("CODER_MAX_STEPS: invalid positive integer %q", v)
		}
		cfg.MaxSteps = n
	}
	if v := strings.TrimSpace(os.Getenv("CODER_COMMAND_TIMEOUT_SEC")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("CODER_COMMAND_TIMEOUT_SEC: invalid positive integer %q", v)
		}
		cfg.CommandTimeoutSec = n
	}
	if v := strings.TrimSpace(os.Getenv("CODER_STREAM")); v != "" {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("CODER_STREAM: %w", err)
		}
		cfg.Streaming = b
	}
	return nil
}

func applyOverrides(cfg *Config, o Overrides) error {
	if v := strings.TrimSpace(o.Provider); v != "" {
		cfg.DefaultProvider = v
	}
	if v := strings.TrimSpace(o.Model); v != "" {
		cfg.DefaultModel = v
	}
	if v := strings.TrimSpace(string(o.ApprovalPolicy)); v != "" {
		pol, err := parseApprovalPolicy(v)
		if err != nil {
			return fmt.Errorf("override approval policy: %w", err)
		}
		cfg.ApprovalPolicy = pol
	} else if o.Safe {
		cfg.ApprovalPolicy = types.ApprovalStrictAsk
	} else if o.FullAuto {
		cfg.ApprovalPolicy = types.ApprovalFullAuto
	}

	if o.CopilotCompat != nil {
		cfg.Compatibility.LoadCopilotInstructions = *o.CopilotCompat
		cfg.Compatibility.LoadCopilotSkills = *o.CopilotCompat
	}
	if o.LoadCopilotInstructions != nil {
		cfg.Compatibility.LoadCopilotInstructions = *o.LoadCopilotInstructions
	}
	if o.LoadCopilotSkills != nil {
		cfg.Compatibility.LoadCopilotSkills = *o.LoadCopilotSkills
	}
	if o.LoadAgentsMD != nil {
		cfg.Compatibility.LoadAgentsMD = *o.LoadAgentsMD
	}

	if o.MaxSteps > 0 {
		cfg.MaxSteps = o.MaxSteps
	}
	if o.MaxOutputBytes > 0 {
		cfg.MaxOutputBytes = o.MaxOutputBytes
	}
	if o.CommandTimeoutSec > 0 {
		cfg.CommandTimeoutSec = o.CommandTimeoutSec
	}
	if v := strings.TrimSpace(o.ReasoningEffort); v != "" {
		cfg.ReasoningEffort = v
	}
	if o.Streaming != nil {
		cfg.Streaming = *o.Streaming
	}
	return nil
}

func parseApprovalPolicy(raw string) (types.ApprovalPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(types.ApprovalSmartAuto):
		return types.ApprovalSmartAuto, nil
	case string(types.ApprovalFullAuto):
		return types.ApprovalFullAuto, nil
	case string(types.ApprovalStrictAsk), "safe":
		return types.ApprovalStrictAsk, nil
	default:
		return "", fmt.Errorf("invalid approval policy %q (expected smart_auto, full_auto, or strict_ask)", raw)
	}
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value %q", raw)
	}
}

func lookupProvider(providers map[string]types.ProviderProfile, name string) (string, types.ProviderProfile, bool) {
	if providers == nil {
		return "", types.ProviderProfile{}, false
	}
	if p, ok := providers[name]; ok {
		return name, p, true
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	for k, p := range providers {
		if strings.ToLower(k) == lower {
			return k, p, true
		}
	}
	return "", types.ProviderProfile{}, false
}

func cloneProviderProfile(p types.ProviderProfile) types.ProviderProfile {
	out := p
	if p.Headers != nil {
		out.Headers = make(map[string]string, len(p.Headers))
		for k, v := range p.Headers {
			out.Headers[k] = v
		}
	}
	if p.WarmSession != nil {
		out.WarmSession = Bool(*p.WarmSession)
	}
	return out
}

func samePath(a, b string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	return strings.EqualFold(ca, cb)
}
