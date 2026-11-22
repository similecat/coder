package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	sdk "github.com/github/copilot-sdk/go"

	"coder/internal/provider/sseutil"
	"coder/internal/types"
)

// ErrNotAuthenticated indicates GitHub Copilot CLI/SDK has no active login or token.
var ErrNotAuthenticated = errors.New("GitHub Copilot is not authenticated")

// LoginRunner overrides interactive `copilot login` subprocess execution in unit tests when non-nil.
var LoginRunner func(ctx context.Context, host string, stdin io.Reader, stdout, stderr io.Writer) error

// IsAuthError reports whether err is caused by missing or invalid GitHub Copilot authentication.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotAuthenticated) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "not created with authentication info") ||
		strings.Contains(lower, "no authentication information") ||
		strings.Contains(lower, "not authenticated") ||
		strings.Contains(lower, "authentication required")
}

func wrapIfAuthError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotAuthenticated) {
		return err
	}
	if IsAuthError(err) {
		return fmt.Errorf(
			"%w: run 'coder login' (or '/login' in REPL) to sign in with GitHub OAuth, set COPILOT_GITHUB_TOKEN, or switch provider (e.g. '-p lmstudio' / '/provider lmstudio')",
			ErrNotAuthenticated,
		)
	}
	return err
}

// BuildEnvWithProxy returns a copy of os.Environ() with HTTPS_PROXY, HTTP_PROXY, and NODE_USE_ENV_PROXY=1 set.
func BuildEnvWithProxy(proxyURL string) []string {
	base := os.Environ()
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return base
	}
	filtered := make([]string, 0, len(base)+3)
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := strings.ToUpper(kv[:eq])
		if k == "HTTPS_PROXY" || k == "HTTP_PROXY" || k == "NODE_USE_ENV_PROXY" {
			continue
		}
		filtered = append(filtered, kv)
	}
	return append(filtered,
		"HTTPS_PROXY="+proxyURL,
		"HTTP_PROXY="+proxyURL,
		"NODE_USE_ENV_PROXY=1",
	)
}

// RunInteractiveLogin launches `copilot login` attached to stdin/stdout/stderr so the user
// can complete GitHub's OAuth device/browser login flow directly inside coder.
func RunInteractiveLogin(ctx context.Context, host string, stdin io.Reader, stdout, stderr io.Writer, explicitProxy ...string) error {
	if LoginRunner != nil {
		return LoginRunner(ctx, host, stdin, stdout, stderr)
	}
	cliPath := ResolveCLIPath()
	if cliPath == "" {
		return errors.New("copilot CLI binary not found on PATH; install @github/copilot first")
	}

	var proxyURL string
	if len(explicitProxy) > 0 {
		proxyURL = strings.TrimSpace(explicitProxy[0])
	}

	args := []string{"login"}
	if strings.TrimSpace(host) != "" {
		args = append(args, "--host", strings.TrimSpace(host))
	}
	cmd := exec.CommandContext(ctx, cliPath, args...)
	if proxyURL != "" {
		cmd.Env = BuildEnvWithProxy(proxyURL)
	} else if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("HTTP_PROXY") != "" || os.Getenv("https_proxy") != "" || os.Getenv("http_proxy") != "" {
		cmd.Env = append(os.Environ(), "NODE_USE_ENV_PROXY=1")
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// sdkClient abstracts *sdk.Client for deterministic unit testing without spawning a live CLI process.
type sdkClient interface {
	Start(ctx context.Context) error
	Stop() error
	ForceStop()
	CreateSession(ctx context.Context, cfg *sdk.SessionConfig) (sdkSession, error)
	ListModels(ctx context.Context) ([]sdk.ModelInfo, error)
	GetAuthStatus(ctx context.Context) (*sdk.GetAuthStatusResponse, error)
}

// sdkSession abstracts *sdk.Session for deterministic unit testing.
type sdkSession interface {
	On(handler sdk.SessionEventHandler) func()
	Send(ctx context.Context, options sdk.MessageOptions) (string, error)
	SendAndWait(ctx context.Context, options sdk.MessageOptions) (*sdk.SessionEvent, error)
	Abort(ctx context.Context) error
	SetModel(ctx context.Context, model string, opts *sdk.SetModelOptions) error
	SetTools(ctx context.Context, tools []sdk.Tool) error
	Disconnect() error
}

type realSDKClient struct {
	c *sdk.Client
}

func (r *realSDKClient) Start(ctx context.Context) error {
	return r.c.Start(ctx)
}

func (r *realSDKClient) Stop() error {
	return r.c.Stop()
}

func (r *realSDKClient) ForceStop() {
	r.c.ForceStop()
}

func (r *realSDKClient) CreateSession(ctx context.Context, cfg *sdk.SessionConfig) (sdkSession, error) {
	return r.c.CreateSession(ctx, cfg)
}

func (r *realSDKClient) ListModels(ctx context.Context) ([]sdk.ModelInfo, error) {
	return r.c.ListModels(ctx)
}

func (r *realSDKClient) GetAuthStatus(ctx context.Context) (*sdk.GetAuthStatusResponse, error) {
	return r.c.GetAuthStatus(ctx)
}

// Provider implements types.ManagedSessionProvider backed by a warm persistent GitHub Copilot SDK client.
type Provider struct {
	profile       types.ProviderProfile
	apiKey        string
	clientFactory func(opts *sdk.ClientOptions) sdkClient

	mu      sync.Mutex
	client  sdkClient
	started bool
	closed  bool
}

var _ types.ManagedSessionProvider = (*Provider)(nil)

// New constructs a GitHub Copilot SDK ManagedSessionProvider.
// The underlying Copilot CLI runtime is started lazily on first CreateSession or ListModels
// and kept warm until Close() is called.
func New(profile types.ProviderProfile) (*Provider, error) {
	apiKey, err := sseutil.ResolveAPIKey(profile)
	if err != nil {
		return nil, err
	}
	return &Provider{
		profile: profile,
		apiKey:  apiKey,
		clientFactory: func(opts *sdk.ClientOptions) sdkClient {
			return &realSDKClient{c: sdk.NewClient(opts)}
		},
	}, nil
}

// Name returns the configured provider profile name (defaulting to "copilot").
func (p *Provider) Name() string {
	if p.profile.Name != "" {
		return p.profile.Name
	}
	return "copilot"
}

// DefaultModel returns the configured default model for this provider.
func (p *Provider) DefaultModel() string {
	return p.profile.DefaultModel
}

// BuildClientOptions returns the low-noise Copilot SDK ClientOptions for the given token.
// It automatically resolves the native Copilot CLI executable on the host so users neither
// need to run the SDK's embedded bundler nor pay the extra cmd.exe + node.exe wrapper latency on Windows.
func BuildClientOptions(apiKey string, baseURL ...string) *sdk.ClientOptions {
	opts := &sdk.ClientOptions{
		LogLevel: "error",
	}
	if strings.TrimSpace(apiKey) != "" {
		opts.GitHubToken = strings.TrimSpace(apiKey)
	}
	var explicitTarget string
	if len(baseURL) > 0 {
		explicitTarget = strings.TrimSpace(baseURL[0])
	}
	if explicitTarget != "" {
		if strings.HasPrefix(explicitTarget, "rpc://") {
			opts.Connection = sdk.URIConnection{URL: strings.TrimPrefix(explicitTarget, "rpc://")}
			return opts
		}
		if !strings.Contains(explicitTarget, "://") {
			opts.Connection = sdk.StdioConnection{
				Path: explicitTarget,
				Args: []string{"--disable-builtin-mcps", "--no-auto-update"},
			}
			return opts
		}
	}
	if cliPath := ResolveCLIPath(); cliPath != "" {
		opts.Connection = sdk.StdioConnection{
			Path: cliPath,
			Args: []string{"--disable-builtin-mcps", "--no-auto-update"},
		}
	}
	return opts
}

var (
	cliPathOnce   sync.Once
	cachedCLIPath string
)

// ResolveCLIPath finds the fastest installed Copilot CLI runtime executable.
// On Windows npm installs, it unwraps copilot.cmd/copilot.ps1 directly to the native
// @github/copilot-win32-<arch>/copilot.exe binary, saving two extra process spawns on startup.
func ResolveCLIPath() string {
	if envPath := strings.TrimSpace(os.Getenv("COPILOT_CLI_PATH")); envPath != "" {
		return envPath
	}
	cliPathOnce.Do(func() {
		cachedCLIPath = discoverCLIPath()
	})
	return cachedCLIPath
}

func discoverCLIPath() string {
	arch := "x64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}

	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("copilot.exe"); err == nil && p != "" {
			return p
		}
		var npmDirs []string
		if p, err := exec.LookPath("copilot"); err == nil && p != "" {
			npmDirs = append(npmDirs, filepath.Dir(p))
		}
		if appData := os.Getenv("APPDATA"); appData != "" {
			npmDirs = append(npmDirs, filepath.Join(appData, "npm"))
		}
		pkgDir := "copilot-win32-" + arch
		for _, dir := range npmDirs {
			candidates := []string{
				filepath.Join(dir, "node_modules", "@github", "copilot", "node_modules", "@github", pkgDir, "copilot.exe"),
				filepath.Join(dir, "node_modules", "@github", pkgDir, "copilot.exe"),
			}
			for _, c := range candidates {
				if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
					return c
				}
			}
		}
	}

	if p, err := exec.LookPath("copilot"); err == nil && p != "" {
		return p
	}
	return ""
}

func (p *Provider) ensureClient(ctx context.Context) (sdkClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, errors.New("copilot provider is closed")
	}
	if p.client != nil && p.started {
		return p.client, nil
	}

	opts := BuildClientOptions(p.apiKey, p.profile.BaseURL)
	if proxyURL := ResolveEffectiveProxy(p.profile.Headers["proxy"]); proxyURL != "" {
		opts.Env = BuildEnvWithProxy(proxyURL)
	}
	c := p.clientFactory(opts)
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("copilot client start: %w", err)
	}
	p.client = c
	p.started = true
	return p.client, nil
}

// IsModelEnabled reports whether a Copilot SDK model entry is enabled by account/org policy
// and suitable for chat/coding (excluding embedding/audio-only models).
func IsModelEnabled(m sdk.ModelInfo) bool {
	if strings.TrimSpace(m.ID) == "" {
		return false
	}
	if m.Policy != nil && m.Policy.State != "" && !strings.EqualFold(m.Policy.State, "enabled") {
		return false
	}
	lower := strings.ToLower(m.ID)
	if strings.Contains(lower, "embed") || strings.Contains(lower, "whisper") ||
		strings.Contains(lower, "tts") || strings.Contains(lower, "rerank") {
		return false
	}
	return true
}

// SelectDefaultModel picks the best available coding model from a Copilot SDK catalog.
func SelectDefaultModel(models []sdk.ModelInfo) string {
	enabled := make([]sdk.ModelInfo, 0, len(models))
	for _, m := range models {
		if IsModelEnabled(m) {
			enabled = append(enabled, m)
		}
	}
	if len(enabled) == 0 {
		if len(models) > 0 {
			return models[0].ID
		}
		return "auto"
	}

	// Preference tiers ordered for coding accuracy and low interactive latency.
	preferredSubstrings := []string{
		"codex",
		"claude-sonnet-4",
		"claude-sonnet",
		"gpt-6.2",
		"gpt-6.1",
		"gpt-6",
		"gpt-5.6-sol-fast",
		"gpt-5.6",
		"gpt-5",
		"gpt-4.1",
		"mai-code-2",
		"gemini-4",
		"gemini-3",
	}

	for _, pat := range preferredSubstrings {
		for _, m := range enabled {
			if !strings.EqualFold(m.ID, "auto") && strings.Contains(strings.ToLower(m.ID), pat) {
				return m.ID
			}
		}
	}

	// Otherwise pick the first concrete (non-"auto") enabled model, falling back to "auto".
	for _, m := range enabled {
		if !strings.EqualFold(m.ID, "auto") {
			return m.ID
		}
	}
	return enabled[0].ID
}

// ResolveAvailableModel validates requested against the live Copilot model catalog.
// When requested is empty/"default" or unavailable in models, it smartly selects the best enabled model.
func ResolveAvailableModel(requested string, models []sdk.ModelInfo) (chosen string, fellBack bool) {
	req := strings.TrimSpace(requested)
	if len(models) == 0 {
		return req, false
	}
	if req == "" || strings.EqualFold(req, "default") {
		return SelectDefaultModel(models), false
	}
	for _, m := range models {
		if IsModelEnabled(m) && strings.EqualFold(m.ID, req) {
			return m.ID, false
		}
	}
	return SelectDefaultModel(models), true
}

// ResolveModel queries the warm Copilot SDK client's model catalog and resolves requested
// (or empty default) to an available model ID.
func (p *Provider) ResolveModel(ctx context.Context, requested string) (chosen string, fellBack bool, err error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return requested, false, err
	}
	if !hasExplicitEnvOrBYOKAuth(p.apiKey, p.profile.BaseURL) {
		if status, authErr := client.GetAuthStatus(ctx); authErr == nil && status != nil && !status.IsAuthenticated {
			return requested, false, wrapIfAuthError(ErrNotAuthenticated)
		}
	}
	rawModels, err := client.ListModels(ctx)
	if err != nil {
		return requested, false, err
	}
	chosen, fellBack = ResolveAvailableModel(requested, rawModels)
	return chosen, fellBack, nil
}

// ListModels queries the warm Copilot SDK client for enabled available models.
func (p *Provider) ListModels(ctx context.Context) ([]types.ModelInfo, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return nil, err
	}
	rawModels, err := client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("copilot list models: %w", err)
	}
	provName := p.Name()
	out := make([]types.ModelInfo, 0, len(rawModels))
	for _, m := range rawModels {
		if !IsModelEnabled(m) {
			continue
		}
		var ctxSize int
		if m.Capabilities.Limits.MaxContextWindowTokens != nil {
			ctxSize = *m.Capabilities.Limits.MaxContextWindowTokens
		} else if m.Capabilities.Limits.MaxPromptTokens != nil {
			ctxSize = *m.Capabilities.Limits.MaxPromptTokens
		}
		out = append(out, types.ModelInfo{
			ID:          m.ID,
			Name:        m.Name,
			Provider:    provName,
			ContextSize: ctxSize,
		})
	}
	return out, nil
}

// AuthStatus queries the Copilot runtime's current authentication state.
func (p *Provider) AuthStatus(ctx context.Context) (*sdk.GetAuthStatusResponse, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetAuthStatus(ctx)
}

func hasExplicitEnvOrBYOKAuth(apiKey, baseURL string) bool {
	if strings.TrimSpace(apiKey) != "" {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(baseURL), "http://") || strings.HasPrefix(strings.TrimSpace(baseURL), "https://") {
		return true
	}
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_PROVIDER_BASE_URL"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// CreateSession starts or reuses the warm Copilot client and opens a low-latency session
// with all heavy Copilot CLI bloat disabled and coder's tools strictly wired in.
func (p *Provider) CreateSession(ctx context.Context, cfg types.ManagedSessionConfig) (types.ManagedSession, error) {
	client, err := p.ensureClient(ctx)
	if err != nil {
		return nil, err
	}

	if !hasExplicitEnvOrBYOKAuth(p.apiKey, p.profile.BaseURL) {
		if status, authErr := client.GetAuthStatus(ctx); authErr == nil && status != nil && !status.IsAuthenticated {
			return nil, wrapIfAuthError(ErrNotAuthenticated)
		}
	}

	requestedModel := strings.TrimSpace(cfg.Model)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(p.profile.DefaultModel)
	}

	var rawModels []sdk.ModelInfo
	isBYOK := strings.HasPrefix(strings.TrimSpace(p.profile.BaseURL), "http://") ||
		strings.HasPrefix(strings.TrimSpace(p.profile.BaseURL), "https://")
	if !isBYOK {
		if listed, listErr := client.ListModels(ctx); listErr == nil && len(listed) > 0 {
			rawModels = listed
			requestedModel, _ = ResolveAvailableModel(requestedModel, rawModels)
		}
	}

	ms := &Session{
		providerName: p.Name(),
		model:        requestedModel,
	}

	sessCfg := BuildSessionConfig(cfg, requestedModel, ms.getHook())
	sessCfg.Model = requestedModel
	if isBYOK {
		sessCfg.Provider = &sdk.ProviderConfig{
			Type:    "openai",
			BaseURL: strings.TrimSpace(p.profile.BaseURL),
			APIKey:  p.apiKey,
		}
	}
	rawSess, err := client.CreateSession(ctx, sessCfg)
	if err != nil && !isBYOK && strings.Contains(strings.ToLower(err.Error()), "is not available") && len(rawModels) > 0 {
		// Fallback cascade if a listed model is still gated at session.create time.
		var fallbacks []string
		for _, m := range rawModels {
			if IsModelEnabled(m) && !strings.EqualFold(m.ID, requestedModel) {
				if strings.EqualFold(m.ID, "auto") {
					fallbacks = append([]string{m.ID}, fallbacks...)
				} else {
					fallbacks = append(fallbacks, m.ID)
				}
			}
		}
		for _, fb := range fallbacks {
			sessCfg.Model = fb
			rawSess, err = client.CreateSession(ctx, sessCfg)
			if err == nil {
				ms.model = fb
				break
			}
			if !strings.Contains(strings.ToLower(err.Error()), "is not available") {
				break
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("copilot create session: %w", wrapIfAuthError(err))
	}
	ms.session = rawSess
	return ms, nil
}

// Close stops the warm Copilot SDK client promptly (with a 600ms graceful drain before ForceStop
// so CLI commands and REPL /quit never stall for 10s on Windows stdio pipe teardown).
func (p *Provider) Close() error {
	p.mu.Lock()
	p.closed = true
	c := p.client
	p.client = nil
	p.started = false
	p.mu.Unlock()

	if c == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- c.Stop()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(600 * time.Millisecond):
		c.ForceStop()
		return nil
	}
}

// ToolInvokeHook allows ManagedSession to intercept tool calls for streaming telemetry and turn context propagation.
type ToolInvokeHook struct {
	Context  func(traceCtx context.Context) (context.Context, context.CancelFunc)
	OnStart  func(toolCallID, toolName, rawArgs string)
	OnFinish func(toolCallID, toolName, rawArgs string, res types.ToolResult)
}

// NormalizeToolArguments converts SDK ToolInvocation.Arguments (string, json.RawMessage, []byte, map[string]any, etc.)
// into valid json.RawMessage without double-quoting JSON strings.
func NormalizeToolArguments(args any) (json.RawMessage, error) {
	if args == nil {
		return json.RawMessage("{}"), nil
	}
	switch v := args.(type) {
	case json.RawMessage:
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) == 0 {
			return json.RawMessage("{}"), nil
		}
		cp := make(json.RawMessage, len(trimmed))
		copy(cp, trimmed)
		return cp, nil
	case []byte:
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) == 0 {
			return json.RawMessage("{}"), nil
		}
		if json.Valid(trimmed) {
			cp := make(json.RawMessage, len(trimmed))
			copy(cp, trimmed)
			return cp, nil
		}
		b, err := json.Marshal(string(v))
		return json.RawMessage(b), err
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return json.RawMessage("{}"), nil
		}
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed), nil
		}
		b, err := json.Marshal(v)
		return json.RawMessage(b), err
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
}

// ConvertTools converts coder's types.Tool slice into Copilot SDK tools and the exact AvailableTools allowlist.
func ConvertTools(tools []types.Tool, hook *ToolInvokeHook) ([]sdk.Tool, []string) {
	sdkTools := make([]sdk.Tool, 0, len(tools))
	toolNames := make([]string, 0, len(tools))

	for _, tool := range tools {
		t := tool
		def := t.Definition()
		toolNames = append(toolNames, def.Name)

		sdkTools = append(sdkTools, sdk.Tool{
			Name:                 def.Name,
			Description:          def.Description,
			Parameters:           def.Parameters,
			OverridesBuiltInTool: true,
			SkipPermission:       true,
			Handler: func(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
				rawArgs, normErr := NormalizeToolArguments(inv.Arguments)
				if normErr != nil {
					errMsg := fmt.Sprintf("invalid tool arguments: %v", normErr)
					return sdk.ToolResult{
						TextResultForLLM: errMsg,
						ResultType:       "failure",
						Error:            errMsg,
					}, nil
				}

				toolName := def.Name
				if toolName == "" {
					toolName = inv.ToolName
				}

				var (
					invCtx = inv.TraceContext
					cancel context.CancelFunc
				)
				if hook != nil && hook.Context != nil {
					invCtx, cancel = hook.Context(inv.TraceContext)
				}
				if invCtx == nil {
					invCtx = context.Background()
				}
				if cancel != nil {
					defer cancel()
				}

				if hook != nil && hook.OnStart != nil {
					hook.OnStart(inv.ToolCallID, toolName, string(rawArgs))
				}

				res, execErr := t.Execute(invCtx, rawArgs)
				if execErr != nil {
					res.IsError = true
					if res.Content == "" {
						res.Content = execErr.Error()
					}
				}

				if hook != nil && hook.OnFinish != nil {
					hook.OnFinish(inv.ToolCallID, toolName, string(rawArgs), res)
				}

				if execErr != nil || res.IsError {
					return sdk.ToolResult{
						TextResultForLLM: res.Content,
						ResultType:       "failure",
						Error:            res.Content,
					}, nil
				}
				return sdk.ToolResult{
					TextResultForLLM: res.Content,
					ResultType:       "success",
				}, nil
			},
		})
	}

	return sdkTools, toolNames
}

// BuildSessionConfig constructs a low-latency Copilot SDK SessionConfig with heavy CLI runtime features disabled.
func BuildSessionConfig(cfg types.ManagedSessionConfig, defaultModel string, hook *ToolInvokeHook) *sdk.SessionConfig {
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}

	sdkTools, toolNames := ConvertTools(cfg.Tools, hook)

	return &sdk.SessionConfig{
		ClientName:             "coder",
		Model:                  model,
		ReasoningEffort:        cfg.ReasoningEffort,
		Streaming:              sdk.Bool(cfg.Streaming),
		AllowMultipleToolCalls: sdk.Bool(true),
		Tools:                  sdkTools,
		AvailableTools:         toolNames,
		IncludedBuiltInAgents:  []string{},
		EnableConfigDiscovery:  sdk.Bool(false),
		SkipCustomInstructions: sdk.Bool(true),
		EnableSkills:           sdk.Bool(false),
		SkipEmbeddingRetrieval: sdk.Bool(true),
		EnableFileHooks:        sdk.Bool(false),
		EnableSessionStore:     sdk.Bool(false),
		OnPermissionRequest:    sdk.PermissionHandler.ApproveAll,
		WorkingDirectory:       cfg.WorkingDir,
		SystemMessage: &sdk.SystemMessageConfig{
			Mode:    "replace",
			Content: cfg.SystemPrompt,
		},
	}
}

type activeTurn struct {
	ctx        context.Context
	onEvent    func(types.StreamEvent)
	start      time.Time
	mu         sync.Mutex
	seenFirst  bool
	gotDelta   bool
	textBuf    strings.Builder
	lastMsgTxt string
	metrics    types.TurnMetrics
}

func (t *activeTurn) markFirstActivityLocked() {
	if !t.seenFirst {
		t.seenFirst = true
		t.metrics.TTFT = time.Since(t.start)
	}
}

func (t *activeTurn) emit(ev types.StreamEvent) {
	if t.onEvent != nil {
		t.onEvent(ev)
	}
}

// Session implements types.ManagedSession over a warm Copilot SDK session.
type Session struct {
	providerName string
	session      sdkSession

	mu         sync.Mutex
	model      string
	turn       *activeTurn
	onToolHook *ToolInvokeHook
}

var _ types.ManagedSession = (*Session)(nil)

func (s *Session) onToolInvokeHook() *ToolInvokeHook {
	return &ToolInvokeHook{
		Context: func(traceCtx context.Context) (context.Context, context.CancelFunc) {
			s.mu.Lock()
			turn := s.turn
			s.mu.Unlock()

			if turn == nil || turn.ctx == nil {
				if traceCtx != nil {
					return traceCtx, nil
				}
				return context.Background(), nil
			}
			if traceCtx == nil {
				return turn.ctx, nil
			}
			mergedCtx, cancel := context.WithCancel(traceCtx)
			stop := context.AfterFunc(turn.ctx, cancel)
			return mergedCtx, func() {
				stop()
				cancel()
			}
		},
		OnStart: func(toolCallID, toolName, rawArgs string) {
			s.mu.Lock()
			turn := s.turn
			s.mu.Unlock()
			if turn == nil {
				return
			}
			turn.mu.Lock()
			turn.markFirstActivityLocked()
			turn.mu.Unlock()
			turn.emit(types.StreamEvent{
				Type:       types.EventToolCallStart,
				ToolCallID: toolCallID,
				ToolName:   toolName,
				ToolArgs:   rawArgs,
			})
		},
		OnFinish: func(toolCallID, toolName, rawArgs string, res types.ToolResult) {
			s.mu.Lock()
			turn := s.turn
			s.mu.Unlock()
			if turn == nil {
				return
			}
			turn.mu.Lock()
			turn.metrics.ToolCalls++
			turn.mu.Unlock()
			resCopy := res
			turn.emit(types.StreamEvent{
				Type:       types.EventToolCallEnd,
				ToolCallID: toolCallID,
				ToolName:   toolName,
				ToolArgs:   rawArgs,
				ToolResult: &resCopy,
			})
		},
	}
}

func initSessionHook(s *Session) *ToolInvokeHook {
	if s.onToolHook == nil {
		s.onToolHook = s.onToolInvokeHook()
	}
	return s.onToolHook
}

// CreateSession helper field binding.
func (s *Session) getHook() *ToolInvokeHook {
	s.mu.Lock()
	defer s.mu.Unlock()
	return initSessionHook(s)
}

// RunTurn sends prompt to the stateful Copilot session, streaming text/reasoning/tool/usage events
// and aborting cleanly without tearing down the warm client if ctx is cancelled.
func (s *Session) RunTurn(ctx context.Context, prompt string, onEvent func(types.StreamEvent)) (string, types.TurnMetrics, error) {
	start := time.Now()

	s.mu.Lock()
	curModel := s.model
	turn := &activeTurn{
		ctx:     ctx,
		onEvent: onEvent,
		start:   start,
		metrics: types.TurnMetrics{
			Provider: s.providerName,
			Model:    curModel,
		},
	}
	s.turn = turn
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.turn == turn {
			s.turn = nil
		}
		s.mu.Unlock()
	}()

	var finishOnce sync.Once
	idleCh := make(chan error, 1)
	signalIdle := func(turnErr error) {
		finishOnce.Do(func() {
			idleCh <- turnErr
		})
	}

	isRootAgentEvent := func(ev sdk.SessionEvent) bool {
		return ev.AgentID == nil || *ev.AgentID == ""
	}

	unsubscribe := s.session.On(func(ev sdk.SessionEvent) {
		switch d := ev.Data.(type) {
		case *sdk.AssistantMessageDeltaData:
			if d == nil || d.DeltaContent == "" || !isRootAgentEvent(ev) {
				return
			}
			turn.mu.Lock()
			turn.markFirstActivityLocked()
			turn.gotDelta = true
			turn.textBuf.WriteString(d.DeltaContent)
			turn.mu.Unlock()
			turn.emit(types.StreamEvent{
				Type: types.EventTextDelta,
				Text: d.DeltaContent,
			})

		case *sdk.AssistantReasoningDeltaData:
			if d == nil || d.DeltaContent == "" || !isRootAgentEvent(ev) {
				return
			}
			turn.mu.Lock()
			turn.markFirstActivityLocked()
			turn.mu.Unlock()
			turn.emit(types.StreamEvent{
				Type: types.EventReasoningDelta,
				Text: d.DeltaContent,
			})

		case *sdk.AssistantTurnStartData:
			if d == nil || !isRootAgentEvent(ev) {
				return
			}
			turn.mu.Lock()
			turn.metrics.LLMCalls++
			if d.Model != nil && *d.Model != "" && turn.metrics.Model == "" {
				turn.metrics.Model = *d.Model
			}
			turn.mu.Unlock()

		case *sdk.AssistantUsageData:
			if d == nil {
				return
			}
			var inTok, outTok, cachedTok int
			if d.InputTokens != nil {
				inTok = int(*d.InputTokens)
			}
			if d.OutputTokens != nil {
				outTok = int(*d.OutputTokens)
			}
			if d.CacheReadTokens != nil {
				cachedTok = int(*d.CacheReadTokens)
			}
			turn.mu.Lock()
			turn.metrics.InputTokens += inTok
			turn.metrics.OutputTokens += outTok
			turn.metrics.CachedTokens += cachedTok
			if d.Model != "" && turn.metrics.Model == "" {
				turn.metrics.Model = d.Model
			}
			turn.mu.Unlock()
			turn.emit(types.StreamEvent{
				Type:         types.EventUsage,
				InputTokens:  inTok,
				OutputTokens: outTok,
				CachedTokens: cachedTok,
			})

		case *sdk.AssistantMessageData:
			if d == nil || !isRootAgentEvent(ev) {
				return
			}
			turn.mu.Lock()
			turn.markFirstActivityLocked()
			turn.lastMsgTxt = d.Content
			turn.mu.Unlock()

		case *sdk.SessionIdleData:
			if !isRootAgentEvent(ev) {
				return
			}
			if d != nil && d.Aborted != nil && *d.Aborted {
				signalIdle(context.Canceled)
				return
			}
			signalIdle(nil)

		case *sdk.SessionAutopilotStoppedData:
			if !isRootAgentEvent(ev) {
				return
			}
			if d != nil && d.Reason == "error" {
				signalIdle(errors.New("copilot session autopilot stopped with error"))
				return
			}
			signalIdle(nil)

		case *sdk.SessionErrorData:
			if !isRootAgentEvent(ev) {
				return
			}
			msg := "unknown session error"
			if d != nil && d.Message != "" {
				msg = d.Message
			}
			signalIdle(fmt.Errorf("session error: %s", msg))
		}
	})
	defer unsubscribe()

	var err error
	if _, sendErr := s.session.Send(ctx, sdk.MessageOptions{Prompt: prompt}); sendErr != nil {
		err = sendErr
	} else {
		select {
		case err = <-idleCh:
		case <-ctx.Done():
			abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.session.Abort(abortCtx)
			cancel()
			err = ctx.Err()
		}
	}

	turn.mu.Lock()
	var finalText string
	if turn.gotDelta {
		finalText = turn.textBuf.String()
	}
	if finalText == "" {
		finalText = turn.lastMsgTxt
	}

	turn.metrics.TotalDuration = time.Since(start)
	if turn.metrics.LLMCalls == 0 && (err == nil || finalText != "" || turn.metrics.InputTokens > 0 || turn.metrics.OutputTokens > 0) {
		turn.metrics.LLMCalls = 1
	}
	metrics := turn.metrics
	turn.mu.Unlock()

	return finalText, metrics, wrapIfAuthError(err)
}

// ActiveModel returns the resolved model ID running on this Copilot session.
func (s *Session) ActiveModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// SetModel switches the model on the active Copilot session.
func (s *Session) SetModel(ctx context.Context, model string) error {
	if err := s.session.SetModel(ctx, model, nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.model = model
	s.mu.Unlock()
	return nil
}

// SetTools updates the tool set registered on the active Copilot session.
func (s *Session) SetTools(ctx context.Context, tools []types.Tool) error {
	sdkTools, _ := ConvertTools(tools, s.getHook())
	return s.session.SetTools(ctx, sdkTools)
}

// Close disconnects the session while keeping the parent Provider's Copilot client warm.
func (s *Session) Close() error {
	return s.session.Disconnect()
}
