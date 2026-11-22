package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
