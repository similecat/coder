package provider

import (
	"io"
	"net/http"

	"coder/internal/provider/sseutil"
	"coder/internal/types"
)

const (
	// MaxSSETokenSize is the 2MB maximum single SSE line size supported by SSEReader.
	MaxSSETokenSize = sseutil.MaxSSETokenSize
)

// SSEEvent represents a single parsed Server-Sent Event frame (`event:` + `data:`).
type SSEEvent = sseutil.SSEEvent

// SSEReader is a zero-bloat, 2MB-buffer Server-Sent Events parser.
type SSEReader = sseutil.SSEReader

// SharedTransport returns the shared low-latency HTTP transport tuned for keep-alive reuse.
func SharedTransport() *http.Transport {
	return sseutil.SharedTransport()
}

// NewHTTPClient returns an HTTP client backed by SharedTransport with no body timeout
// so long SSE streams never time out prematurely while honoring ctx.Done().
func NewHTTPClient() *http.Client {
	return sseutil.NewHTTPClient()
}

// NewSSEReader creates a new SSEReader with a 2MB max token buffer.
func NewSSEReader(r io.Reader) *SSEReader {
	return sseutil.NewSSEReader(r)
}

// IsLocalProvider reports whether profile targets a local inference endpoint
// (127.0.0.1, localhost, Ollama, llama.cpp, vLLM, LM Studio).
func IsLocalProvider(profile types.ProviderProfile) bool {
	return sseutil.IsLocalProvider(profile)
}

// ResolveAPIKey checks profile.APIKey, then os.Getenv(profile.APIKeyEnv), allowing empty
// API keys for local providers and logged-in Copilot SDK sessions.
func ResolveAPIKey(profile types.ProviderProfile) (string, error) {
	return sseutil.ResolveAPIKey(profile)
}
