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
