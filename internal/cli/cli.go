package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"coder/internal/bootstrap"
	"coder/internal/config"
	"coder/internal/provider/copilot"
	"coder/internal/repl"
	"coder/internal/types"
)

// Version is the semantic version reported by `coder --version`.
const Version = "0.1.0"

// BenchRunnerFunc is the callback signature delegated to by `coder bench [args...]`.
type BenchRunnerFunc func(args []string, stdout, stderr io.Writer) int

// ExecJSONOutput represents the machine-readable JSON output emitted by `coder exec --json`.
type ExecJSONOutput struct {
	Output   string            `json:"output"`
	Metrics  types.TurnMetrics `json:"metrics"`
	Provider string            `json:"provider"`
	Model    string            `json:"model"`
	Error    string            `json:"error,omitempty"`
}

// RunOptions configures a full CLI execution, allowing optional test injection of
// CustomProvider, UserHome, and Interactive state without mutating global OS state.
type RunOptions struct {
	Args           []string
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	BenchFn        BenchRunnerFunc
	CustomProvider types.Provider
	UserHome       string
	Interactive    *bool
}

// Execute parses CLI arguments and dispatches to repl, exec, doctor, config, or bench.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer, benchFn BenchRunnerFunc) int {
	return ExecuteWithOptions(RunOptions{
		Args:    args,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		BenchFn: benchFn,
	})
}

// ExecuteWithOptions runs the CLI dispatcher with optional test overrides.
func ExecuteWithOptions(opts RunOptions) int {
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Fast delegation for `coder bench [args...]` before general flag parsing so bench's own
	// flags (including --help, --json, --problems, etc.) are forwarded intact.
	if isBench, benchArgs := extractBenchInvocation(opts.Args); isBench {
		if opts.BenchFn == nil {
			_, _ = fmt.Fprintln(stderr, "coder bench: benchmark runner is not registered")
			return 1
		}
		return opts.BenchFn(benchArgs, stdout, stderr)
	}

	parsed, err := parseArgs(opts.Args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder: %v\nRun 'coder --help' for usage.\n", err)
		return 2
	}

	if parsed.showVersion {
		_, _ = fmt.Fprintf(stdout, "coder v%s\n", Version)
		return 0
	}
	if parsed.showHelp {
		printHelp(stdout, parsed.subcommand)
		return 0
	}

	wsDir, err := resolveWorkspaceDir(parsed.cwd)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder: %v\n", err)
		return 1
	}

	switch parsed.subcommand {
	case "version":
		_, _ = fmt.Fprintf(stdout, "coder v%s\n", Version)
		return 0

	case "help":
		topic := ""
		if len(parsed.rest) > 0 {
			topic = parsed.rest[0]
		}
		printHelp(stdout, topic)
		return 0

	case "config":
		return runConfigCommand(wsDir, opts.UserHome, parsed, stdout, stderr)

	case "doctor":
		return runDoctorCommand(wsDir, opts.UserHome, parsed, stdout, stderr)

	case "login":
		return runLoginCommand(wsDir, opts.UserHome, parsed, stdin, stdout, stderr)

	case "exec", "run":
		return runExecCommand(wsDir, opts, parsed, stdin, stdout, stderr)

	case "repl", "chat":
		return runREPLCommand(wsDir, opts, parsed, stdin, stdout, stderr)

	case "":
		if parsed.execMode || len(parsed.rest) > 0 {
			return runExecCommand(wsDir, opts, parsed, stdin, stdout, stderr)
		}
		return runREPLCommand(wsDir, opts, parsed, stdin, stdout, stderr)

	default:
		_, _ = fmt.Fprintf(stderr, "coder: unknown subcommand %q\n", parsed.subcommand)
		return 2
	}
}

type parsedCLI struct {
	subcommand      string
	rest            []string
	cwd             string
	loginHost       string
	loginProxy      string
	loginToken      string
	loginTokenPrompt bool
	overrides       config.Overrides

	execMode    bool
	jsonOutput  bool
	quiet       bool
	noColor     bool
	userScope   bool
	force       bool
	showVersion bool
	showHelp    bool
}

// extractBenchInvocation detects `coder [flags...] bench [args...]` and forwards all non-"bench"
// arguments directly to BenchRunnerFunc so internal/bench controls its own flag set.
func extractBenchInvocation(args []string) (bool, []string) {
	if len(args) == 0 {
		return false, nil
	}
	if args[0] == "bench" {
		return true, args[1:]
	}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			return false, nil
		}
		if strings.HasPrefix(tok, "-") {
			name, _, hasEq := splitFlagToken(tok)
			switch name {
			case "-e", "--exec", "-v", "--version", "-h", "--help":
				return false, nil
			case "-p", "--provider", "-m", "--model", "-C", "--cwd",
				"--base-url", "--api-key", "--max-steps", "--reasoning-effort":
				if !hasEq {
					i++
				}
			}
			continue
		}
		if tok == "bench" {
			forwarded := make([]string, 0, len(args)-1)
			forwarded = append(forwarded, args[:i]...)
			forwarded = append(forwarded, args[i+1:]...)
			return true, forwarded
		}
		return false, nil
	}
	return false, nil
}

func parseArgs(args []string) (parsedCLI, error) {
	var p parsedCLI
	var positionals []string

	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}

		// Single "-" is standard stdin sentinel, not a flag.
		if tok == "-" || !strings.HasPrefix(tok, "-") {
			positionals = append(positionals, tok)
			continue
		}

		name, val, hasEq := splitFlagToken(tok)

		nextVal := func(flagLabel string) (string, error) {
			if hasEq {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s requires a value", flagLabel)
			}
			i++
			return args[i], nil
		}

		parseBoolOpt := func(defaultVal bool) (bool, error) {
			if hasEq {
				return parseCLIBool(val)
			}
			return defaultVal, nil
		}

		switch name {
		case "-v", "--version":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.showVersion = b

		case "-h", "--help":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.showHelp = b

		case "-p", "--provider":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.overrides.Provider = v

		case "-m", "--model":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.overrides.Model = v

		case "-C", "--cwd":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.cwd = v

		case "--full-auto", "--yolo":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.overrides.FullAuto = b

		case "--safe":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.overrides.Safe = b

		case "--copilot-compat":
			var b bool
			var err error
			if hasEq {
				b, err = parseCLIBool(val)
			} else if i+1 < len(args) && (strings.EqualFold(args[i+1], "true") || strings.EqualFold(args[i+1], "false")) {
				i++
				b, err = parseCLIBool(args[i])
			} else {
				b = true
			}
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.overrides.CopilotCompat = config.Bool(b)

		case "--no-copilot-compat":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.overrides.CopilotCompat = config.Bool(!b)

		case "--base-url":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.overrides.BaseURL = v

		case "--api-key":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.overrides.APIKey = v

		case "--max-steps":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			n, convErr := strconv.Atoi(v)
			if convErr != nil || n <= 0 {
				return p, fmt.Errorf("--max-steps requires a positive integer, got %q", v)
			}
			p.overrides.MaxSteps = n

		case "--reasoning-effort":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.overrides.ReasoningEffort = v

		case "--stream", "--streaming":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.overrides.Streaming = config.Bool(b)

		case "-e", "--exec":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.execMode = b

		case "--json":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.jsonOutput = b

		case "-q", "--quiet":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.quiet = b

		case "--no-color":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.noColor = b

		case "--user":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.userScope = b

		case "-f", "--force":
			b, err := parseBoolOpt(true)
			if err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
			p.force = b

		case "--host":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.loginHost = v

		case "--proxy":
			v, err := nextVal(name)
			if err != nil {
				return p, err
			}
			p.loginProxy = v

		case "--token":
			if hasEq {
				p.loginToken = val
				p.loginTokenPrompt = strings.TrimSpace(val) == ""
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				p.loginToken = args[i]
			} else {
				p.loginTokenPrompt = true
			}

		default:
			return p, fmt.Errorf("unknown flag %q", name)
		}
	}

	if len(positionals) > 0 && !p.execMode && isKnownSubcommand(positionals[0]) {
		p.subcommand = strings.ToLower(positionals[0])
		p.rest = positionals[1:]
	} else {
		p.rest = positionals
	}

	return p, nil
}

func isKnownSubcommand(tok string) bool {
	switch strings.ToLower(tok) {
	case "repl", "chat", "exec", "run", "login", "doctor", "config", "bench", "version", "help":
		return true
	default:
		return false
	}
}

func splitFlagToken(tok string) (name, val string, hasEq bool) {
	if eq := strings.IndexByte(tok, '='); eq > 0 {
		return tok[:eq], tok[eq+1:], true
	}
	return tok, "", false
}

func parseCLIBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", raw)
	}
}

func resolveWorkspaceDir(cwdFlag string) (string, error) {
	ws := strings.TrimSpace(cwdFlag)
	if ws == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		ws = cwd
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path for %q: %w", ws, err)
	}
	return abs, nil
}

func runLoginCommand(
	wsDir string,
	userHome string,
	parsed parsedCLI,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	token := strings.TrimSpace(parsed.loginToken)
	if token == "" && parsed.loginTokenPrompt {
		_, _ = fmt.Fprint(stderr, "Paste GitHub token (github_pat_... with 'Copilot Requests' permission): ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		token = strings.TrimSpace(line)
		if token == "" {
			_, _ = fmt.Fprintln(stderr, "coder login: cancelled (empty token)")
			return 1
		}
	}

	if token != "" {
		loginName, savedPath, err := verifyAndSaveCopilotToken(ctx, wsDir, userHome, token, parsed.loginProxy)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "coder login: %v\n", err)
			return 1
		}
		if loginName != "" {
			_, _ = fmt.Fprintf(stdout, "Authenticated with GitHub Copilot as @%s (saved to %s).\n", loginName, savedPath)
		} else {
			_, _ = fmt.Fprintf(stdout, "Saved GitHub Copilot token to %s.\n", savedPath)
		}
		return 0
	}

	var err error
	if copilot.LoginRunner != nil {
		err = copilot.LoginRunner(ctx, parsed.loginHost, stdin, stdout, stderr)
		if err == nil {
			return 0
		}
	} else {
		var res *copilot.DeviceLoginResult
		res, err = copilot.RunDeviceFlowLogin(ctx, copilot.DeviceLoginOptions{
			Host:     parsed.loginHost,
			ProxyURL: parsed.loginProxy,
			Stdout:   stdout,
			Stderr:   stderr,
		})
		if err == nil && res != nil && res.Token != "" {
			loginName, savedPath, saveErr := verifyAndSaveCopilotToken(ctx, wsDir, userHome, res.Token, parsed.loginProxy)
			if saveErr != nil {
				_, _ = fmt.Fprintf(stderr, "coder login: %v\n", saveErr)
				return 1
			}
			if loginName == "" {
				loginName = res.Login
			}
			if loginName != "" {
				_, _ = fmt.Fprintf(stdout, "Signed in successfully as @%s (token saved to %s).\n", loginName, savedPath)
			} else {
				_, _ = fmt.Fprintf(stdout, "Signed in successfully (token saved to %s).\n", savedPath)
			}
			return 0
		}
	}

	// When github.com:443 is blocked on networks without a terminal proxy (causing fetch failed),
	// api.github.com & api.githubcopilot.com often still work directly! Offer interactive PAT fallback.
	if isCharDevice(stdin) {
		_, _ = fmt.Fprintf(
			stderr,
			"\n[note] OAuth device flow to https://github.com/login/device/code failed (%v).\n"+
				"On networks where github.com is blocked without a terminal proxy, you can either:\n"+
				"  1. Re-run with your local proxy:  coder login --proxy http://127.0.0.1:<port>\n"+
				"  2. Or paste a Fine-Grained PAT (https://github.com/settings/personal-access-tokens/new\n"+
				"     with Account Permission -> 'Copilot Requests: Read') — uses api.github.com directly with NO proxy needed!\n\n"+
				"Paste GitHub token now (or press Enter to exit): ",
			err,
		)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		pasted := strings.TrimSpace(line)
		if pasted != "" {
			loginName, savedPath, saveErr := verifyAndSaveCopilotToken(ctx, wsDir, userHome, pasted, parsed.loginProxy)
			if saveErr != nil {
				_, _ = fmt.Fprintf(stderr, "coder login: %v\n", saveErr)
				return 1
			}
			if loginName != "" {
				_, _ = fmt.Fprintf(stdout, "Authenticated with GitHub Copilot as @%s (saved to %s).\n", loginName, savedPath)
			} else {
				_, _ = fmt.Fprintf(stdout, "Saved GitHub Copilot token to %s.\n", savedPath)
			}
			return 0
		}
	}

	_, _ = fmt.Fprintf(stderr, "coder login: %v\n", err)
	return 1
}

func verifyAndSaveCopilotToken(
	ctx context.Context,
	wsDir string,
	userHome string,
	token string,
	proxyURL string,
) (loginName string, savedPath string, err error) {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "ghp_") {
		return "", "", errors.New("classic PATs (ghp_...) are not supported by Copilot CLI; create a fine-grained PAT (github_pat_...) with Account Permission -> 'Copilot Requests: Read'")
	}

	prof := types.ProviderProfile{
		Name:         "copilot",
		Type:         types.ProviderTypeCopilotSDK,
		APIKey:       token,
		DefaultModel: "",
	}
	if strings.TrimSpace(proxyURL) != "" {
		prof.Headers = map[string]string{"proxy": strings.TrimSpace(proxyURL)}
	}

	prov, err := copilot.New(prof)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = prov.Close() }()

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if st, stErr := prov.AuthStatus(checkCtx); stErr == nil && st != nil {
		if st.Login != nil {
			loginName = *st.Login
		}
	}

	cfg, err := config.Load(wsDir, userHome, config.Overrides{})
	if err != nil {
		cfg = config.DefaultConfig()
	}
	existing, ok := cfg.Providers["copilot"]
	if !ok {
		existing = prof
	} else {
		existing.APIKey = token
		if strings.TrimSpace(proxyURL) != "" {
			if existing.Headers == nil {
				existing.Headers = make(map[string]string)
			}
			existing.Headers["proxy"] = strings.TrimSpace(proxyURL)
		}
	}
	cfg.Providers["copilot"] = existing

	savedPath, err = config.UserConfigPath(userHome)
	if err != nil {
		return "", "", err
	}
	if err := config.SaveUserConfig(userHome, cfg); err != nil {
		return "", "", err
	}
	return loginName, savedPath, nil
}

func runREPLCommand(
	wsDir string,
	opts RunOptions,
	parsed parsedCLI,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir:   wsDir,
		UserHome:       opts.UserHome,
		Overrides:      parsed.overrides,
		CustomProvider: opts.CustomProvider,
		Interactive:    opts.Interactive,
		Stdin:          stdin,
		Stderr:         stderr,
		WarmToolchains: true,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder: %v\n", err)
		return 1
	}
	defer func() { _ = rt.Close() }()

	err = repl.Run(context.Background(), rt, repl.Options{
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		NoColor: parsed.noColor,
		Version: Version,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder: %v\n", err)
		return 1
	}
	return 0
}

func runExecCommand(
	wsDir string,
	opts RunOptions,
	parsed parsedCLI,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	promptText, stdinConsumed, err := resolveExecPrompt(parsed.rest, stdin)
	if err != nil {
		if parsed.jsonOutput {
			emitExecJSON(stdout, ExecJSONOutput{Error: err.Error()})
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "coder exec: %v\n", err)
		return 2
	}

	// Determine whether stdin is an interactive TTY available for permission prompts.
	// When non-interactive without --full-auto, smart_auto still auto-approves reads & workspace
	// edits while safely denying unapproved shell commands.
	interactive := false
	if opts.Interactive != nil {
		interactive = *opts.Interactive
	} else if !stdinConsumed && isCharDevice(stdin) {
		interactive = true
	}

	var gateStdin io.Reader
	if interactive {
		gateStdin = stdin
	}

	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir:   wsDir,
		UserHome:       opts.UserHome,
		Overrides:      parsed.overrides,
		CustomProvider: opts.CustomProvider,
		Interactive:    &interactive,
		Stdin:          gateStdin,
		Stderr:         stderr,
		WarmToolchains: parsed.overrides.FullAuto || interactive,
	})
	if err != nil {
		if parsed.jsonOutput {
			emitExecJSON(stdout, ExecJSONOutput{Error: err.Error()})
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "coder exec: %v\n", err)
		return 1
	}
	defer func() { _ = rt.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	noColor := parsed.noColor || os.Getenv("NO_COLOR") != ""

	if parsed.jsonOutput {
		var progressWriter io.Writer
		if !parsed.quiet {
			progressWriter = stderr
		}
		finalText, metrics, turnErr := repl.StreamTurn(ctx, rt.Agent, promptText, nil, progressWriter, parsed.quiet, noColor)
		outObj := ExecJSONOutput{
			Output:   finalText,
			Metrics:  metrics,
			Provider: metrics.Provider,
			Model:    metrics.Model,
		}
		if outObj.Provider == "" {
			outObj.Provider = rt.Agent.ProviderName()
		}
		if outObj.Model == "" {
			outObj.Model = rt.Agent.Model()
		}
		if turnErr != nil {
			outObj.Error = turnErr.Error()
			emitExecJSON(stdout, outObj)
			return 1
		}
		emitExecJSON(stdout, outObj)
		return 0
	}

	_, _, turnErr := repl.StreamTurn(ctx, rt.Agent, promptText, stdout, stderr, parsed.quiet, noColor)
	if turnErr != nil {
		_, _ = fmt.Fprintf(stderr, "coder exec: %v\n", turnErr)
		return 1
	}
	return 0
}

func emitExecJSON(stdout io.Writer, payload ExecJSONOutput) {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func resolveExecPrompt(args []string, stdin io.Reader) (prompt string, stdinConsumed bool, err error) {
	readFromStdin := len(args) == 0 || (len(args) == 1 && args[0] == "-")
	if !readFromStdin {
		joined := strings.TrimSpace(strings.Join(args, " "))
		if joined == "" {
			return "", false, errors.New("prompt cannot be empty")
		}
		return joined, false, nil
	}

	if stdin == nil || (len(args) == 0 && isCharDevice(stdin)) {
		return "", false, errors.New("prompt is required (pass prompt arguments or pipe via stdin)")
	}

	raw, readErr := io.ReadAll(stdin)
	if readErr != nil {
		return "", true, fmt.Errorf("read stdin prompt: %w", readErr)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", true, errors.New("prompt read from stdin is empty")
	}
	return trimmed, true, nil
}

func runConfigCommand(
	wsDir string,
	userHome string,
	parsed parsedCLI,
	stdout io.Writer,
	stderr io.Writer,
) int {
	action := "show"
	if len(parsed.rest) > 0 {
		action = strings.ToLower(strings.TrimSpace(parsed.rest[0]))
	}

	switch action {
	case "show":
		cfg, err := config.Load(wsDir, userHome, parsed.overrides)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "coder config show: %v\n", err)
			return 1
		}
		// Resolve effective API keys on cloned provider profiles, then redact every secret before printing.
		redacted := redactConfigForDisplay(cfg)
		encoded, err := json.MarshalIndent(redacted, "", "  ")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "coder config show: %v\n", err)
			return 1
		}
		_, _ = stdout.Write(append(encoded, '\n'))
		return 0

	case "path", "paths":
		userPath, userErr := config.UserConfigPath(userHome)
		if userErr != nil {
			userPath = fmt.Sprintf("(unavailable: %v)", userErr)
		}
		wsPath := config.WorkspaceConfigPath(wsDir)
		_, _ = fmt.Fprintf(stdout, "user:      %s\nworkspace: %s\n", userPath, wsPath)
		return 0

	case "init":
		return runConfigInit(wsDir, userHome, parsed, stdout, stderr)

	default:
		_, _ = fmt.Fprintf(stderr, "coder config: unknown action %q (expected show, init, or path)\n", action)
		return 2
	}
}

func redactConfigForDisplay(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	cp := *cfg
	if len(cfg.Providers) > 0 {
		cp.Providers = make(map[string]types.ProviderProfile, len(cfg.Providers))
		for k, prof := range cfg.Providers {
			cloned := prof
			// Also check if ResolveProvider resolves an env key so callers know a key is configured without leaking it.
			if strings.TrimSpace(cloned.APIKey) != "" {
				cloned.APIKey = "***REDACTED***"
			}
			if len(prof.Headers) > 0 {
				cloned.Headers = make(map[string]string, len(prof.Headers))
				for hk, hv := range prof.Headers {
					lowerHK := strings.ToLower(hk)
					if hv != "" && (strings.Contains(lowerHK, "authorization") || strings.Contains(lowerHK, "api-key") || strings.Contains(lowerHK, "token")) {
						cloned.Headers[hk] = "***REDACTED***"
					} else {
						cloned.Headers[hk] = hv
					}
				}
			}
			cp.Providers[k] = cloned
		}
	}
	return &cp
}

func runConfigInit(
	wsDir string,
	userHome string,
	parsed parsedCLI,
	stdout io.Writer,
	stderr io.Writer,
) int {
	starter := config.DefaultConfig()
	if strings.TrimSpace(parsed.overrides.Provider) != "" {
		starter.DefaultProvider = strings.TrimSpace(parsed.overrides.Provider)
	}
	if strings.TrimSpace(parsed.overrides.Model) != "" {
		starter.DefaultModel = strings.TrimSpace(parsed.overrides.Model)
	}
	if parsed.overrides.Safe {
		starter.ApprovalPolicy = types.ApprovalStrictAsk
	} else if parsed.overrides.FullAuto {
		starter.ApprovalPolicy = types.ApprovalFullAuto
	}
	if parsed.overrides.CopilotCompat != nil {
		starter.Compatibility.LoadCopilotInstructions = *parsed.overrides.CopilotCompat
		starter.Compatibility.LoadCopilotSkills = *parsed.overrides.CopilotCompat
	}
	if parsed.overrides.Streaming != nil {
		starter.Streaming = *parsed.overrides.Streaming
	}

	if parsed.userScope {
		targetPath, err := config.UserConfigPath(userHome)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "coder config init: %v\n", err)
			return 1
		}
		if _, statErr := os.Stat(targetPath); statErr == nil && !parsed.force {
			_, _ = fmt.Fprintf(stderr, "coder config init: %s already exists (use --force to overwrite)\n", targetPath)
			return 1
		}
		if err := config.SaveUserConfig(userHome, starter); err != nil {
			_, _ = fmt.Fprintf(stderr, "coder config init: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "Wrote user config: %s\n", targetPath)
		return 0
	}

	targetPath := config.WorkspaceConfigPath(wsDir)
	if _, statErr := os.Stat(targetPath); statErr == nil && !parsed.force {
		_, _ = fmt.Fprintf(stderr, "coder config init: %s already exists (use --force to overwrite)\n", targetPath)
		return 1
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "coder config init: create %s: %v\n", dir, err)
		return 1
	}

	encoded, err := json.MarshalIndent(starter, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder config init: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')

	if err := writeFileAtomicInDir(targetPath, encoded); err != nil {
		_, _ = fmt.Fprintf(stderr, "coder config init: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "Wrote workspace config: %s\n", targetPath)
	return 0
}

func writeFileAtomicInDir(targetPath string, content []byte) error {
	dir := filepath.Dir(targetPath)
	tmpFile, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(content); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func isCharDevice(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok || f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func printHelp(w io.Writer, topic string) {
	switch strings.ToLower(strings.TrimSpace(topic)) {
	case "exec", "run":
		_, _ = fmt.Fprint(w, `Usage: coder exec [flags] [prompt...]

Run a headless single-shot coding turn. Reads prompt from positional arguments
or from piped stdin when prompt arguments are omitted or "-".

Flags:
  -p, --provider <name>          Provider profile (copilot, ollama, local, openai, anthropic, ...)
  -m, --model <id>               Model identifier override
  -C, --cwd <dir>                Workspace directory
      --full-auto, --yolo        Auto-approve all tool calls including shell commands
      --safe                     Require confirmation for all edits and commands (strict_ask)
      --copilot-compat=<bool>    Enable/disable GitHub Copilot instructions & skills
      --no-copilot-compat        Disable GitHub Copilot instructions & skills
      --json                     Emit structured JSON result {output, metrics, provider, model, error}
  -q, --quiet                    Suppress tool progress and latency telemetry on stderr
      --max-steps <n>            Maximum tool-call iterations per turn
      --reasoning-effort <lvl>   Reasoning effort hint
      --stream[=<true|false>]    Enable/disable provider streaming (default: false)
`)
	case "config":
		_, _ = fmt.Fprint(w, `Usage: coder config [show|init|path] [flags]

Subcommands:
  show     Print resolved merged JSON configuration (API keys redacted)
  init     Write a starter .coder/config.json (--user for ~/.coder/config.json, --force to overwrite)
  path     Print user and workspace configuration file paths
`)
	case "login":
		_, _ = fmt.Fprint(w, `Usage: coder login [--token [<github_pat>]] [--proxy <url>] [--host <url>]

Authenticate with GitHub Copilot:
  coder login                                  Interactive browser/device OAuth flow
  coder login --proxy http://127.0.0.1:7890    OAuth flow via HTTP/HTTPS proxy
  coder login --token github_pat_...           Save a Fine-Grained PAT ('Copilot Requests: Read') to ~/.coder/config.json
  coder login --token                          Prompt interactively to paste a token
`)
	case "doctor":
		_, _ = fmt.Fprint(w, `Usage: coder doctor [flags]

Inspect configuration, Copilot/native customizations, providers, and local toolchains.
`)
	default:
		_, _ = fmt.Fprintf(w, `coder v%s — High-speed multi-provider CLI coding agent

Usage:
  coder [flags]                       Start interactive low-latency REPL
  coder -e "prompt"                   Run headless single-shot prompt
  coder exec [flags] [prompt...]      Run headless single-shot prompt (alias: run)
  coder repl [flags]                  Start interactive REPL (alias: chat)
  coder login [--host <url>]          Sign in to GitHub Copilot via OAuth device/browser flow
  coder doctor [flags]                Run environment, provider, and toolchain diagnostics
  coder config [show|init|path]       Inspect or initialize .coder configuration
  coder bench [flags]                 Run automated C++ / Python coding benchmark suite

Global Flags:
  -p, --provider <name>               Active provider profile (default: copilot)
  -m, --model <id>                    Model override
  -C, --cwd <dir>                     Workspace directory
      --full-auto, --yolo             Approve all edits and commands automatically
      --safe                          Confirm all edits and commands (strict_ask)
      --copilot-compat=<true|false>   Toggle GitHub Copilot instructions & skills
      --no-copilot-compat             Disable GitHub Copilot instructions & skills
      --base-url <url>                Override provider endpoint URL
      --api-key <key>                 Override provider API key
      --max-steps <n>                 Max agent steps per turn
      --reasoning-effort <level>      Model reasoning effort
      --stream[=<true|false>]         Enable/disable provider streaming (default: false)
  -e, --exec                          Execute single-shot prompt instead of REPL
      --json                          JSON output mode (for exec / doctor)
  -q, --quiet                         Suppress progress / telemetry on stderr
      --no-color                      Disable ANSI terminal styling
  -v, --version                       Print version
  -h, --help                          Show help
`, Version)
	}
}
