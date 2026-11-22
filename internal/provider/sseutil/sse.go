package sseutil

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"coder/internal/types"
)

const (
	// InitialSSEBufferSize is the initial token buffer allocated per SSE stream.
	InitialSSEBufferSize = 64 * 1024
	// MaxSSETokenSize is the 2MB maximum single SSE line size so large tool-call chunks never overflow.
	MaxSSETokenSize = 2 * 1024 * 1024
)

var (
	transportOnce   sync.Once
	sharedTransport *http.Transport
)

// SharedTransport returns a process-wide low-latency HTTP transport tuned for
// connection keep-alive reuse without a response-body timeout that kills long streams.
func SharedTransport() *http.Transport {
	transportOnce.Do(func() {
		sharedTransport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	})
	return sharedTransport
}

// NewHTTPClient returns an HTTP client backed by SharedTransport with no body timeout
// so long-running SSE streams run uninterrupted until EOF or ctx.Done().
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: SharedTransport(),
	}
}

// SSEEvent represents a single parsed Server-Sent Event frame.
type SSEEvent struct {
	Event string
	Data  string
}

// SSEReader is a zero-bloat, large-buffer Server-Sent Events parser.
type SSEReader struct {
	scanner *bufio.Scanner
	dataBuf strings.Builder
	done    bool
}

// NewSSEReader wraps r in a 2MB-capable SSE stream parser.
func NewSSEReader(r io.Reader) *SSEReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, InitialSSEBufferSize), MaxSSETokenSize)
	return &SSEReader{scanner: sc}
}

// Next reads and returns the next complete SSE event, or io.EOF when the stream ends.
func (r *SSEReader) Next() (SSEEvent, error) {
	if r.done {
		return SSEEvent{}, io.EOF
	}

	r.dataBuf.Reset()
	var eventType string
	hasData := false

	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 {
			if hasData {
				ev := SSEEvent{
					Event: eventType,
					Data:  r.dataBuf.String(),
				}
				r.dataBuf.Reset()
				return ev, nil
			}
			eventType = ""
			continue
		}

		// Ignore SSE comment / keep-alive lines starting with ':'
		if line[0] == ':' {
			continue
		}

		colonIdx := bytes.IndexByte(line, ':')
		var field, val []byte
		if colonIdx < 0 {
			field = line
		} else {
			field = line[:colonIdx]
			val = line[colonIdx+1:]
			// Per WHATWG SSE spec, strip at most one leading U+0020 SPACE after ':'.
			if len(val) > 0 && val[0] == ' ' {
				val = val[1:]
			}
		}

		if bytes.Equal(field, []byte("data")) {
			if hasData {
				r.dataBuf.WriteByte('\n')
			}
			r.dataBuf.Write(val)
			hasData = true
		} else if bytes.Equal(field, []byte("event")) {
			eventType = string(val)
		}
	}

	r.done = true
	if err := r.scanner.Err(); err != nil {
		return SSEEvent{}, err
	}
	if hasData {
		ev := SSEEvent{
			Event: eventType,
			Data:  r.dataBuf.String(),
		}
		r.dataBuf.Reset()
		return ev, nil
	}
	return SSEEvent{}, io.EOF
}

// IsLocalProvider reports whether profile targets a local inference server
// (e.g. 127.0.0.1, localhost, Ollama, llama.cpp, vLLM, LM Studio) that may run without an API key.
func IsLocalProvider(profile types.ProviderProfile) bool {
	if rawURL := strings.TrimSpace(profile.BaseURL); rawURL != "" {
		if u, err := url.Parse(rawURL); err == nil {
			host := strings.ToLower(u.Hostname())
			if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" ||
				host == "host.docker.internal" || strings.HasSuffix(host, ".local") {
				return true
			}
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return true
			}
		}
	}

	combined := strings.ToLower(profile.Name + " " + profile.BaseURL)
	localHints := []string{
		"ollama",
		"llama.cpp",
		"llamacpp",
		"llama-server",
		"vllm",
		"lmstudio",
		"lm studio",
		"lm-studio",
		"lm_studio",
		"localai",
	}
	for _, hint := range localHints {
		if strings.Contains(combined, hint) {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(profile.Name), "local")
}

// ResolveAPIKey resolves the API key from profile.APIKey, then os.Getenv(profile.APIKeyEnv)
// (with standard provider env fallbacks when APIKeyEnv is unset for remote providers),
// allowing empty keys for Copilot SDK (which supports logged-in user auth) and local servers.
func ResolveAPIKey(profile types.ProviderProfile) (string, error) {
	if key := strings.TrimSpace(profile.APIKey); key != "" {
		return key, nil
	}
	if envName := strings.TrimSpace(profile.APIKeyEnv); envName != "" {
		if key := strings.TrimSpace(os.Getenv(envName)); key != "" {
			return key, nil
		}
	} else if !IsLocalProvider(profile) {
		switch profile.Type {
		case types.ProviderTypeAnthropic:
			if key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); key != "" {
				return key, nil
			}
		case types.ProviderTypeOpenAICompat:
			if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
				return key, nil
			}
		}
	}

	if profile.Type == types.ProviderTypeCopilotSDK || IsLocalProvider(profile) {
		return "", nil
	}

	label := profile.Name
	if label == "" {
		label = string(profile.Type)
	}
	if profile.APIKeyEnv != "" {
		return "", fmt.Errorf("provider %q: missing API key (environment variable %s is empty)", label, profile.APIKeyEnv)
	}
	return "", fmt.Errorf("provider %q: missing API key", label)
}
