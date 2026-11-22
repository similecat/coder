package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"coder/internal/types"
)

func TestNew_Dispatch(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-ant-test-123")

	t.Run("copilot_sdk implements ManagedSessionProvider", func(t *testing.T) {
		p, err := New(types.ProviderProfile{
			Name:         "gh-copilot",
			Type:         types.ProviderTypeCopilotSDK,
			DefaultModel: "gpt-5.4",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer p.Close()

		if _, ok := p.(types.ManagedSessionProvider); !ok {
			t.Fatalf("expected copilot provider to implement types.ManagedSessionProvider, got %T", p)
		}
		if p.Name() != "gh-copilot" {
			t.Fatalf("expected name gh-copilot, got %q", p.Name())
		}
		if p.DefaultModel() != "gpt-5.4" {
			t.Fatalf("expected default model gpt-5.4, got %q", p.DefaultModel())
		}
	})

	t.Run("openai_compat local ollama implements ChatProvider without API key", func(t *testing.T) {
		p, err := New(types.ProviderProfile{
			Name:         "ollama",
			Type:         types.ProviderTypeOpenAICompat,
			BaseURL:      "http://127.0.0.1:11434/v1",
			DefaultModel: "qwen2.5-coder:32b",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer p.Close()

		if _, ok := p.(types.ChatProvider); !ok {
			t.Fatalf("expected openai_compat provider to implement types.ChatProvider, got %T", p)
		}
	})

	t.Run("anthropic implements ChatProvider with env key", func(t *testing.T) {
		p, err := New(types.ProviderProfile{
			Name:         "claude",
			Type:         types.ProviderTypeAnthropic,
			APIKeyEnv:    "TEST_ANTHROPIC_KEY",
			DefaultModel: "claude-sonnet-4-20250514",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer p.Close()

		if _, ok := p.(types.ChatProvider); !ok {
			t.Fatalf("expected anthropic provider to implement types.ChatProvider, got %T", p)
		}
	})

	t.Run("unsupported provider type fails", func(t *testing.T) {
		_, err := New(types.ProviderProfile{
			Name: "invalid",
			Type: types.ProviderType("unknown_backend"),
		})
		if err == nil {
			t.Fatal("expected error for unsupported provider type")
		}
	})
}

func TestResolveAPIKeyAndLocalProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CUSTOM_PROVIDER_KEY", "env-secret-key")

	// Explicit APIKey takes priority over APIKeyEnv
	key, err := ResolveAPIKey(types.ProviderProfile{
		Type:      types.ProviderTypeOpenAICompat,
		BaseURL:   "https://api.openai.com/v1",
		APIKey:    "explicit-secret",
		APIKeyEnv: "CUSTOM_PROVIDER_KEY",
	})
	if err != nil || key != "explicit-secret" {
		t.Fatalf("expected explicit-secret, got key=%q err=%v", key, err)
	}

	// Fallback to APIKeyEnv
	key, err = ResolveAPIKey(types.ProviderProfile{
		Type:      types.ProviderTypeOpenAICompat,
		BaseURL:   "https://openrouter.ai/api/v1",
		APIKeyEnv: "CUSTOM_PROVIDER_KEY",
	})
	if err != nil || key != "env-secret-key" {
		t.Fatalf("expected env-secret-key, got key=%q err=%v", key, err)
	}

	// Local endpoints allow empty API keys
	localProfiles := []types.ProviderProfile{
		{Name: "local-ip", Type: types.ProviderTypeOpenAICompat, BaseURL: "http://127.0.0.1:8080/v1"},
		{Name: "local-host", Type: types.ProviderTypeOpenAICompat, BaseURL: "http://localhost:1234/v1"},
		{Name: "ollama", Type: types.ProviderTypeOpenAICompat},
		{Name: "llama.cpp", Type: types.ProviderTypeOpenAICompat, BaseURL: "http://gpu-box:8080/v1"},
		{Name: "vllm-cluster", Type: types.ProviderTypeOpenAICompat, BaseURL: "http://10.0.0.5:8000/v1"},
		{Name: "LM Studio", Type: types.ProviderTypeOpenAICompat, BaseURL: "http://192.168.1.10:1234/v1"},
	}
	for _, lp := range localProfiles {
		if !IsLocalProvider(lp) {
			t.Errorf("expected IsLocalProvider=true for %+v", lp)
		}
		k, err := ResolveAPIKey(lp)
		if err != nil || k != "" {
			t.Errorf("expected empty allowed key for %+v, got k=%q err=%v", lp, k, err)
		}
	}

	// Remote provider without key returns error
	_, err = ResolveAPIKey(types.ProviderProfile{
		Name:      "remote-openai",
		Type:      types.ProviderTypeOpenAICompat,
		BaseURL:   "https://api.openai.com/v1",
		APIKeyEnv: "MISSING_KEY_ENV_VAR",
	})
	if err == nil {
		t.Fatal("expected error for missing remote API key")
	}
}

func TestSharedTransportAndHTTPClient(t *testing.T) {
	tr := SharedTransport()
	if tr.MaxIdleConnsPerHost != 16 {
		t.Fatalf("expected MaxIdleConnsPerHost=16, got %d", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Fatalf("expected IdleConnTimeout=90s, got %v", tr.IdleConnTimeout)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("expected TLSHandshakeTimeout=10s, got %v", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("expected ResponseHeaderTimeout=0, got %v", tr.ResponseHeaderTimeout)
	}

	client := NewHTTPClient()
	if client.Timeout != 0 {
		t.Fatalf("expected HTTPClient Timeout=0 so long SSE streams are not killed, got %v", client.Timeout)
	}
}

func TestSSEReader_LargePayloadAndEdgeCases(t *testing.T) {
	largeVal := strings.Repeat("x", 512*1024) // 512KB > default 64KB bufio.Scanner limit
	rawStream := strings.Join([]string{
		": keep-alive comment",
		"",
		"event: message_start",
		"data: {\"hello\":\"world\"}",
		"",
		"event: multiline",
		"data: line1",
		"data:  line2-with-leading-space",
		"",
		"data: " + largeVal,
		"",
		"event: trailing_without_blank",
		"data: final_payload",
	}, "\r\n")

	reader := NewSSEReader(strings.NewReader(rawStream))

	ev1, err := reader.Next()
	if err != nil || ev1.Event != "message_start" || ev1.Data != `{"hello":"world"}` {
		t.Fatalf("unexpected ev1: %+v err=%v", ev1, err)
	}

	ev2, err := reader.Next()
	if err != nil || ev2.Event != "multiline" || ev2.Data != "line1\n line2-with-leading-space" {
		t.Fatalf("unexpected ev2: %+v err=%v", ev2, err)
	}

	ev3, err := reader.Next()
	if err != nil || ev3.Event != "" || len(ev3.Data) != len(largeVal) {
		t.Fatalf("unexpected ev3 len=%d err=%v", len(ev3.Data), err)
	}

	ev4, err := reader.Next()
	if err != nil || ev4.Event != "trailing_without_blank" || ev4.Data != "final_payload" {
		t.Fatalf("unexpected ev4: %+v err=%v", ev4, err)
	}

	_, err = reader.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}
