package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"coder/internal/types"
)

// SessionSnapshot captures the effective session settings to persist into the user profile (~/.coder/config.json).
type SessionSnapshot struct {
	Provider          string
	Model             string
	ApprovalPolicy    types.ApprovalPolicy
	MaxSteps          int
	MaxOutputBytes    int
	CommandTimeoutSec int
	ReasoningEffort   string
	Streaming         bool
	Compatibility     Compatibility
	ProviderProfile   *types.ProviderProfile
}

// SaveUserCopilotModel patches only the Copilot model in the on-disk user config.
func SaveUserCopilotModel(userHome, model string) (string, error) {
	return SaveUserProviderModel(userHome, "copilot", model)
}

// SaveUserProviderModel patches only providers.<provider>.default_model in the on-disk user config,
// preserving all unrelated settings, provider profiles, and unknown JSON keys.
func SaveUserProviderModel(userHome, provider, model string) (string, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "", errors.New("provider name cannot be empty")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("model name cannot be empty")
	}
	path, root, err := readUserConfigRoot(userHome)
	if err != nil {
		return "", err
	}
	providersKey, err := userConfigKey(root, "providers")
	if err != nil {
		return "", err
	}
	providers, err := userConfigObject(root[providersKey])
	if err != nil {
		return "", fmt.Errorf("parse %s providers: %w", path, err)
	}
	provKey, err := userConfigKey(providers, provider)
	if err != nil {
		return "", err
	}
	profile, err := userConfigObject(providers[provKey])
	if err != nil {
		return "", fmt.Errorf("parse %s %s profile: %w", path, provider, err)
	}
	modelKey, err := userConfigKey(profile, "default_model")
	if err != nil {
		return "", err
	}
	// RawMessage preserves the values of keys that the typed config does not know about.
	profile[modelKey], _ = json.Marshal(model)
	providers[provKey], _ = json.Marshal(profile)
	root[providersKey], _ = json.Marshal(providers)
	if err := writeUserConfig(path, root); err != nil {
		return "", err
	}
	return path, nil
}

// SaveUserSessionConfig surgically patches the user profile (<userHome>/.coder/config.json)
// with snap so future sessions start with the same effective configuration while preserving
// unrelated provider profiles, stored API keys, and custom/unknown JSON fields.
func SaveUserSessionConfig(userHome string, snap SessionSnapshot) (string, error) {
	prov := strings.TrimSpace(snap.Provider)
	if prov == "" {
		return "", errors.New("provider name cannot be empty")
	}

	policy := snap.ApprovalPolicy
	if strings.TrimSpace(string(policy)) == "" {
		policy = types.ApprovalSmartAuto
	} else {
		parsed, err := parseApprovalPolicy(string(policy))
		if err != nil {
			return "", err
		}
		policy = parsed
	}

	path, root, err := readUserConfigRoot(userHome)
	if err != nil {
		return "", err
	}

	if err := setUserConfigValue(root, "default_provider", prov); err != nil {
		return "", err
	}
	// Clear top-level default_model so per-provider default_model governs the active provider
	// without breaking future switches to other providers (`coder -p <other>`).
	if err := deleteUserConfigKey(root, "default_model"); err != nil {
		return "", err
	}
	if err := setUserConfigValue(root, "approval_policy", string(policy)); err != nil {
		return "", err
	}
	if snap.MaxSteps > 0 {
		if err := setUserConfigValue(root, "max_steps", snap.MaxSteps); err != nil {
			return "", err
		}
	}
	if snap.MaxOutputBytes > 0 {
		if err := setUserConfigValue(root, "max_output_bytes", snap.MaxOutputBytes); err != nil {
			return "", err
		}
	}
	if snap.CommandTimeoutSec > 0 {
		if err := setUserConfigValue(root, "command_timeout_sec", snap.CommandTimeoutSec); err != nil {
			return "", err
		}
	}
	if effort := strings.TrimSpace(snap.ReasoningEffort); effort != "" {
		if err := setUserConfigValue(root, "reasoning_effort", effort); err != nil {
			return "", err
		}
	} else {
		if err := deleteUserConfigKey(root, "reasoning_effort"); err != nil {
			return "", err
		}
	}
	if err := setUserConfigValue(root, "streaming", snap.Streaming); err != nil {
		return "", err
	}

	compatKey, err := userConfigKey(root, "compatibility")
	if err != nil {
		return "", err
	}
	compatObj, err := userConfigObject(root[compatKey])
	if err != nil {
		return "", fmt.Errorf("parse %s compatibility: %w", path, err)
	}
	if err := setUserConfigValue(compatObj, "load_copilot_instructions", snap.Compatibility.LoadCopilotInstructions); err != nil {
		return "", err
	}
	if err := setUserConfigValue(compatObj, "load_copilot_skills", snap.Compatibility.LoadCopilotSkills); err != nil {
		return "", err
	}
	if err := setUserConfigValue(compatObj, "load_agents_md", snap.Compatibility.LoadAgentsMD); err != nil {
		return "", err
	}
	root[compatKey], _ = json.Marshal(compatObj)

	providersKey, err := userConfigKey(root, "providers")
	if err != nil {
		return "", err
	}
	providers, err := userConfigObject(root[providersKey])
	if err != nil {
		return "", fmt.Errorf("parse %s providers: %w", path, err)
	}
	provKey, err := userConfigKey(providers, prov)
	if err != nil {
		return "", err
	}
	profile, err := userConfigObject(providers[provKey])
	if err != nil {
		return "", fmt.Errorf("parse %s %s profile: %w", path, prov, err)
	}

	if err := setUserConfigValue(profile, "default_model", strings.TrimSpace(snap.Model)); err != nil {
		return "", err
	}

	if snap.ProviderProfile != nil {
		if err := patchProviderProfileNonSecret(path, prov, profile, *snap.ProviderProfile); err != nil {
			return "", err
		}
	}

	providers[provKey], _ = json.Marshal(profile)
	root[providersKey], _ = json.Marshal(providers)

	if err := writeUserConfig(path, root); err != nil {
		return "", err
	}
	return path, nil
}

func patchProviderProfileNonSecret(
	cfgPath string,
	provName string,
	profile map[string]json.RawMessage,
	src types.ProviderProfile,
) error {
	_, builtin, isBuiltin := lookupProvider(DefaultProviders(), provName)

	if src.Type != "" && (!isBuiltin || src.Type != builtin.Type || hasUserConfigKey(profile, "type")) {
		if err := setUserConfigValue(profile, "type", src.Type); err != nil {
			return err
		}
	}
	if strings.TrimSpace(src.BaseURL) != strings.TrimSpace(builtin.BaseURL) || hasUserConfigKey(profile, "base_url") {
		if err := setUserConfigValue(profile, "base_url", strings.TrimSpace(src.BaseURL)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(src.APIKeyEnv) != strings.TrimSpace(builtin.APIKeyEnv) || hasUserConfigKey(profile, "api_key_env") {
		if err := setUserConfigValue(profile, "api_key_env", strings.TrimSpace(src.APIKeyEnv)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(src.APIVersion) != strings.TrimSpace(builtin.APIVersion) || hasUserConfigKey(profile, "api_version") {
		if err := setUserConfigValue(profile, "api_version", strings.TrimSpace(src.APIVersion)); err != nil {
			return err
		}
	}
	if src.TimeoutSec != builtin.TimeoutSec || hasUserConfigKey(profile, "timeout_sec") {
		if err := setUserConfigValue(profile, "timeout_sec", src.TimeoutSec); err != nil {
			return err
		}
	}
	if !equalBoolPtr(src.WarmSession, builtin.WarmSession) || hasUserConfigKey(profile, "warm_session") {
		if src.WarmSession != nil {
			if err := setUserConfigValue(profile, "warm_session", *src.WarmSession); err != nil {
				return err
			}
		}
	}

	// Merge only non-sensitive headers; never overwrite or expose credentials.
	var safeHeaders map[string]string
	for hk, hv := range src.Headers {
		if !IsSensitiveHeaderKey(hk) {
			if safeHeaders == nil {
				safeHeaders = make(map[string]string)
			}
			safeHeaders[hk] = hv
		}
	}
	if len(safeHeaders) > 0 {
		headersKey, err := userConfigKey(profile, "headers")
		if err != nil {
			return err
		}
		headersObj, err := userConfigObject(profile[headersKey])
		if err != nil {
			return fmt.Errorf("parse %s %s headers: %w", cfgPath, provName, err)
		}
		for hk, hv := range safeHeaders {
			headersObj[hk], _ = json.Marshal(hv)
		}
		profile[headersKey], _ = json.Marshal(headersObj)
	}
	return nil
}

func readUserConfigRoot(userHome string) (string, map[string]json.RawMessage, error) {
	path, err := UserConfigPath(userHome)
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("read config %s: %w", path, err)
	}
	root := make(map[string]json.RawMessage)
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return "", nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		if root == nil {
			return "", nil, fmt.Errorf("expected JSON object in %s", path)
		}
	}
	return path, root, nil
}

func setUserConfigValue(obj map[string]json.RawMessage, name string, val any) error {
	key, err := userConfigKey(obj, name)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(val)
	if err != nil {
		return err
	}
	obj[key] = encoded
	return nil
}

func deleteUserConfigKey(obj map[string]json.RawMessage, name string) error {
	key, err := userConfigKey(obj, name)
	if err != nil {
		return err
	}
	delete(obj, key)
	return nil
}

func hasUserConfigKey(obj map[string]json.RawMessage, name string) bool {
	for k := range obj {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func equalBoolPtr(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func userConfigObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
	}
	if obj == nil {
		obj = make(map[string]json.RawMessage)
	}
	return obj, nil
}

func userConfigKey(obj map[string]json.RawMessage, name string) (string, error) {
	match := ""
	for key := range obj {
		if strings.EqualFold(key, name) {
			if match != "" {
				return "", fmt.Errorf("ambiguous config key %q", name)
			}
			match = key
		}
	}
	if match == "" {
		return name, nil
	}
	return match, nil
}
