package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SaveUserCopilotModel patches only the Copilot model in the on-disk user config.
func SaveUserCopilotModel(userHome, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("model name cannot be empty")
	}
	path, err := UserConfigPath(userHome)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read config %s: %w", path, err)
	}
	root := make(map[string]json.RawMessage)
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return "", fmt.Errorf("parse config %s: %w", path, err)
		}
		if root == nil {
			return "", fmt.Errorf("expected JSON object in %s", path)
		}
	}
	providersKey, err := userConfigKey(root, "providers")
	if err != nil {
		return "", err
	}
	providers, err := userConfigObject(root[providersKey])
	if err != nil {
		return "", fmt.Errorf("parse %s providers: %w", path, err)
	}
	copilotKey, err := userConfigKey(providers, "copilot")
	if err != nil {
		return "", err
	}
	profile, err := userConfigObject(providers[copilotKey])
	if err != nil {
		return "", fmt.Errorf("parse %s Copilot profile: %w", path, err)
	}
	modelKey, err := userConfigKey(profile, "default_model")
	if err != nil {
		return "", err
	}
	// RawMessage preserves the values of keys that the typed config does not know about.
	profile[modelKey], _ = json.Marshal(model)
	providers[copilotKey], _ = json.Marshal(profile)
	root[providersKey], _ = json.Marshal(providers)
	if err := writeUserConfig(path, root); err != nil {
		return "", err
	}
	return path, nil
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
