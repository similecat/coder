package copilot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"coder/internal/config"
	"coder/internal/types"
	sdk "github.com/github/copilot-sdk/go"
)

func TestSavedModelFallsBackToAutoAndIsRetriedInNewSessions(t *testing.T) {
	t.Setenv("CODER_PROVIDER", "")
	t.Setenv("CODER_MODEL", "")
	t.Setenv("CODER_BASE_URL", "")
	const preferred = "gpt-6.2-sol"
	home := t.TempDir()
	path, err := config.SaveUserCopilotModel(home, preferred)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		listed bool
		gated  bool
		models []string
	}{
		{name: "missing from catalog", models: []string{"auto"}},
		{name: "listed but gated at session creation", listed: true, gated: true, models: []string{preferred, "auto"}},
		{name: "available again", listed: true, models: []string{preferred}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load("", home, config.Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := cfg.ResolveProvider("copilot")
			if err != nil {
				t.Fatal(err)
			}
			p, err := New(profile)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			var attempts []string
			fc := &fakeSDKClient{
				catalog: []sdk.ModelInfo{{ID: "gpt-5.3-codex"}, {ID: "auto"}},
				createErr: func(model string) error {
					attempts = append(attempts, model)
					if tc.gated && model == preferred {
						return errors.New("model is not available")
					}
					return nil
				},
				sessHook: func(cfg *sdk.SessionConfig) *fakeSDKSession { return &fakeSDKSession{cfg: cfg} },
			}
			if tc.listed {
				fc.catalog = append(fc.catalog, sdk.ModelInfo{ID: preferred})
			}
			p.clientFactory = func(*sdk.ClientOptions) sdkClient { return fc }
			ms, err := p.CreateSession(context.Background(), types.ManagedSessionConfig{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ms.Close() })
			if active := ms.(*Session).ActiveModel(); active != tc.models[len(tc.models)-1] || !slices.Equal(attempts, tc.models) {
				t.Fatalf("active=%q, creation attempts=%v, want attempts %v", active, attempts, tc.models)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("session changed saved preference: %v", err)
			}
		})
	}
}
