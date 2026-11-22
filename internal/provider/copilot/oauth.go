package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	// CopilotOAuthClientID is GitHub Copilot CLI's public OAuth App Client ID for RFC 8628 Device Flow.
	CopilotOAuthClientID = "Ov23ctDVkRmgkPke0Mmm"
	// CopilotOAuthScope matches GitHub Copilot CLI's requested OAuth scopes.
	CopilotOAuthScope = "read:user,read:org,repo,gist"
)

// DeviceCodeResponse holds GitHub's /login/device/code payload.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DeviceLoginOptions configures native Go OAuth Device Flow authentication.
type DeviceLoginOptions struct {
	Host         string        // Defaults to "https://github.com"
	APIBaseURL   string        // Optional override for GitHub REST API user lookup in tests
	ClientID     string        // Defaults to CopilotOAuthClientID
	ProxyURL     string        // Optional explicit HTTP/HTTPS proxy URL
	PollInterval time.Duration // Optional poll interval override for tests
	Stdout       io.Writer
	Stderr       io.Writer
	SkipBrowser  bool // Disables launching OS browser (used in unit tests)
}

// DeviceLoginResult holds the authenticated token and GitHub username.
type DeviceLoginResult struct {
	Token string
	Login string
	Host  string
}

// ResolveEffectiveProxy returns explicitProxy if non-empty, then HTTP(S)_PROXY env vars,
// and on Windows falls back to the active Windows System Proxy (Internet Settings registry) when enabled.
func ResolveEffectiveProxy(explicitProxy string) string {
	if p := normalizeProxyURL(explicitProxy); p != "" {
		return p
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := normalizeProxyURL(os.Getenv(k)); v != "" {
			return v
		}
	}
	if runtime.GOOS == "windows" {
		return detectWindowsSystemProxy()
	}
	return ""
}

func normalizeProxyURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Handle Windows multi-protocol format: "http=127.0.0.1:7890;https=127.0.0.1:7890"
	if strings.Contains(s, "=") {
		var fallback string
		for _, part := range strings.Split(s, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			schemeKey := strings.ToLower(strings.TrimSpace(kv[0]))
			val := strings.TrimSpace(kv[1])
			if val == "" {
				continue
			}
			if schemeKey == "https" {
				s = val
				break
			}
			if fallback == "" && (schemeKey == "http" || schemeKey == "socks" || schemeKey == "socks5") {
				if schemeKey == "socks" || schemeKey == "socks5" {
					fallback = "socks5://" + val
				} else {
					fallback = val
				}
			}
		}
		if strings.Contains(s, "=") && fallback != "" {
			s = fallback
		}
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return s
}

func detectWindowsSystemProxy() string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	const regKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	out, err := exec.CommandContext(ctx, "reg.exe", "query", regKey).Output()
	if err != nil {
		return ""
	}

	var enabled bool
	var server string
	for _, rawLine := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(rawLine))
		if len(fields) < 3 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "proxyenable":
			val := strings.ToLower(fields[len(fields)-1])
			enabled = val == "0x1" || val == "1"
		case "proxyserver":
			server = fields[len(fields)-1]
		}
	}
	if !enabled || server == "" {
		return ""
	}
	return normalizeProxyURL(server)
}

func newOAuthHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	tr := &http.Transport{
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	}
	effectiveProxy := ResolveEffectiveProxy(proxyURL)
	if effectiveProxy != "" {
		parsed, err := url.Parse(effectiveProxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", effectiveProxy, err)
		}
		tr.Proxy = http.ProxyURL(parsed)
	} else {
		tr.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
	}, nil
}

// RunDeviceFlowLogin executes GitHub's OAuth Device Flow natively in Go with resilient timeouts,
// automatic retries, Windows System Proxy detection, clipboard copy, and a prominent terminal code display
// so the one-time user_code is ALWAYS clearly visible in coder's output before/while the browser opens.
func RunDeviceFlowLogin(ctx context.Context, opts DeviceLoginOptions) (*DeviceLoginResult, error) {
	host := strings.TrimRight(strings.TrimSpace(opts.Host), "/")
	if host == "" {
		host = "https://github.com"
	} else if !strings.HasPrefix(strings.ToLower(host), "http://") && !strings.HasPrefix(strings.ToLower(host), "https://") {
		host = "https://" + host
	}

	clientID := strings.TrimSpace(opts.ClientID)
	if clientID == "" {
		clientID = CopilotOAuthClientID
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	effectiveProxy := ResolveEffectiveProxy(opts.ProxyURL)
	if effectiveProxy != "" {
		_, _ = fmt.Fprintf(stderr, "[network] using proxy %s\n", effectiveProxy)
	}

	client, err := newOAuthHTTPClient(opts.ProxyURL, 30*time.Second)
	if err != nil {
		return nil, err
	}

	_, _ = fmt.Fprintf(stdout, "Requesting one-time device code from %s ...\n", host)

	dc, err := requestDeviceCodeWithRetry(ctx, client, host, clientID, stderr)
	if err != nil {
		return nil, err
	}

	copied := false
	if !opts.SkipBrowser {
		copied = copyToClipboard(dc.UserCode)
	}

	// Print the device code in a bold, unmissable banner BEFORE launching the browser
	// so the code is always prominently visible in coder's terminal output!
	_, _ = fmt.Fprintf(stdout, "\n============================================================\n")
	_, _ = fmt.Fprintf(stdout, "  One-Time Device Code :  %s\n", dc.UserCode)
	_, _ = fmt.Fprintf(stdout, "  Verification URL     :  %s\n", dc.VerificationURI)
	_, _ = fmt.Fprintf(stdout, "============================================================\n")
	if copied {
		_, _ = fmt.Fprintf(stdout, "(Code %s copied to clipboard! Paste it in your browser.)\n", dc.UserCode)
	} else {
		_, _ = fmt.Fprintf(stdout, "(Enter code %s at %s in your browser.)\n", dc.UserCode, dc.VerificationURI)
	}

	if !opts.SkipBrowser {
		if openErr := openBrowserURL(dc.VerificationURI); openErr != nil {
			_, _ = fmt.Fprintf(stderr, "[note] could not auto-open browser (%v); open %s manually.\n", openErr, dc.VerificationURI)
		}
	}

	_, _ = fmt.Fprintln(stdout, "Waiting for GitHub authorization in your browser (press Ctrl+C to cancel)...")
	if f, ok := stdout.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	if s, ok := stdout.(interface{ Sync() error }); ok {
		_ = s.Sync()
	}

	token, err := pollAccessToken(ctx, client, host, clientID, dc, opts.PollInterval, stderr)
	if err != nil {
		return nil, err
	}

	login := fetchGitHubUsername(ctx, client, host, opts.APIBaseURL, token)
	return &DeviceLoginResult{
		Token: token,
		Login: login,
		Host:  host,
	}, nil
}

func requestDeviceCodeWithRetry(
	ctx context.Context,
	client *http.Client,
	host string,
	clientID string,
	stderr io.Writer,
) (*DeviceCodeResponse, error) {
	endpoint := host + "/login/device/code"
	form := url.Values{
		"client_id": {clientID},
		"scope":     {CopilotOAuthScope},
	}.Encode()

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "coder-cli")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 3 {
				_, _ = fmt.Fprintf(stderr, "[retry %d/3] %s slow/unreachable (%v), retrying...\n", attempt, endpoint, err)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Second):
				}
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("device code HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var dc DeviceCodeResponse
		if err := json.Unmarshal(body, &dc); err != nil {
			return nil, fmt.Errorf("parse device code response: %w", err)
		}
		if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" {
			return nil, fmt.Errorf("incomplete device code response: %s", strings.TrimSpace(string(body)))
		}
		if dc.ExpiresIn <= 0 {
			dc.ExpiresIn = 900
		}
		if dc.Interval <= 0 {
			dc.Interval = 5
		}
		return &dc, nil
	}
	return nil, fmt.Errorf("request device code from %s failed after 3 attempts: %w", endpoint, lastErr)
}

func pollAccessToken(
	ctx context.Context,
	client *http.Client,
	host string,
	clientID string,
	dc *DeviceCodeResponse,
	overrideInterval time.Duration,
	stderr io.Writer,
) (string, error) {
	endpoint := host + "/login/oauth/access_token"
	interval := time.Duration(dc.Interval+1) * time.Second
	if overrideInterval > 0 {
		interval = overrideInterval
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	form := url.Values{
		"client_id":   {clientID},
		"device_code": {dc.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}.Encode()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}

		reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(form))
		if err != nil {
			cancel()
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "coder-cli")

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// Transient network timeout during polling: keep polling instead of aborting the user's code!
			_, _ = fmt.Fprintf(stderr, "[polling] transient network error (%v); continuing to wait for code %s...\n", err, dc.UserCode)
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			continue
		}

		var payload struct {
			AccessToken      string `json:"access_token"`
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			Interval         int    `json:"interval"`
		}
		if json.Unmarshal(body, &payload) != nil {
			continue
		}

		if strings.TrimSpace(payload.AccessToken) != "" {
			return strings.TrimSpace(payload.AccessToken), nil
		}

		switch payload.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			if payload.Interval > 0 {
				interval = time.Duration(payload.Interval+2) * time.Second
			} else {
				interval += 5 * time.Second
			}
		case "expired_token":
			return "", errors.New("device code expired before authorization completed; run 'coder login' again")
		case "access_denied":
			return "", errors.New("authorization was denied in the browser")
		default:
			if payload.ErrorDescription != "" {
				return "", errors.New(payload.ErrorDescription)
			}
			if payload.Error != "" {
				return "", fmt.Errorf("oauth token error: %s", payload.Error)
			}
		}
	}

	return "", errors.New("device code expired before authorization completed")
}

func fetchGitHubUsername(ctx context.Context, client *http.Client, host, apiBaseOverride, token string) string {
	apiBase := strings.TrimRight(strings.TrimSpace(apiBaseOverride), "/")
	if apiBase == "" {
		if strings.EqualFold(host, "https://github.com") {
			apiBase = "https://api.github.com"
		} else {
			apiBase = host + "/api/v3"
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, apiBase+"/user", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "coder-cli")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var user struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&user) == nil {
		return user.Login
	}
	return ""
}

func copyToClipboard(text string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(ctx, "clip.exe")
	case "darwin":
		cmd = exec.CommandContext(ctx, "pbcopy")
	default:
		return false
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}

func openBrowserURL(targetURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", targetURL)
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", targetURL)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", targetURL)
	}
	return cmd.Start()
}
