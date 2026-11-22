package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunDeviceFlowLoginDisplaysDeviceCodeAndPollsToCompletion(t *testing.T) {
	var pollCount atomic.Int32

	mux := http.NewServeMux()
	var srvURL string

	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev_code_123",
			"user_code":        "ABCD-9876",
			"verification_uri": srvURL + "/login/device",
			"expires_in":       60,
			"interval":         0,
		})
	})

	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		n := pollCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "authorization_pending",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gho_mock_oauth_token_xyz",
			"token_type":   "bearer",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_mock_oauth_token_xyz" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login": "octocat",
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	var stdout, stderr bytes.Buffer
	res, err := RunDeviceFlowLogin(context.Background(), DeviceLoginOptions{
		Host:         srv.URL,
		APIBaseURL:   srv.URL,
		PollInterval: 5 * time.Millisecond,
		Stdout:       &stdout,
		Stderr:       &stderr,
		SkipBrowser:  true,
	})
	if err != nil {
		t.Fatalf("RunDeviceFlowLogin failed: %v (stderr: %s)", err, stderr.String())
	}

	if res.Token != "gho_mock_oauth_token_xyz" || res.Login != "octocat" {
		t.Fatalf("unexpected DeviceLoginResult: %+v", res)
	}
	if pollCount.Load() < 2 {
		t.Fatalf("expected >=2 polls (authorization_pending -> access_token), got %d", pollCount.Load())
	}

	out := stdout.String()
	if !strings.Contains(out, "One-Time Device Code :  ABCD-9876") {
		t.Fatalf("expected prominent device code banner in stdout, got:\n%s", out)
	}
	if !strings.Contains(out, srv.URL+"/login/device") {
		t.Fatalf("expected verification URL in stdout, got:\n%s", out)
	}
}

func TestNormalizeProxyURL(t *testing.T) {
	cases := map[string]string{
		"":                                            "",
		"127.0.0.1:7890":                              "http://127.0.0.1:7890",
		"http://127.0.0.1:7890":                       "http://127.0.0.1:7890",
		"http=127.0.0.1:7890;https=127.0.0.1:7891":    "http://127.0.0.1:7891",
		"socks=127.0.0.1:1080":                        "socks5://127.0.0.1:1080",
	}
	for in, want := range cases {
		if got := normalizeProxyURL(in); got != want {
			t.Errorf("normalizeProxyURL(%q) = %q, want %q", in, got, want)
		}
	}
}
