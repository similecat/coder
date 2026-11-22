package repl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"coder/internal/agent"
	"coder/internal/bootstrap"
	"coder/internal/provider/copilot"
	"coder/internal/types"
)

// DefaultVersion is the version string displayed in the REPL startup header when Options.Version is unset.
const DefaultVersion = "0.1.0"

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiCyan  = "\x1b[36m"
	ansiRed   = "\x1b[31m"
)

// Options configures an interactive REPL session.
type Options struct {
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
	NoColor         bool
	Version         string
	InterruptCh     <-chan os.Signal // Optional signal stream (in addition to os.Interrupt) for deterministic tests
	AllowAuthPrompt *bool            // Optional override for interactive Copilot login/fallback menu in tests
}

type sessionState struct {
	rt          *bootstrap.Runtime
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	reader      *bufio.Reader
	noColor     bool
	version     string
	queuedSkill string
}

// Run starts the interactive low-latency REPL loop on rt.
func Run(ctx context.Context, rt *bootstrap.Runtime, opts Options) error {
	if rt == nil || rt.Agent == nil {
		return errors.New("repl requires an initialized bootstrap.Runtime with a non-nil Agent")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	in := opts.Stdin
	if in == nil {
		in = os.Stdin
	}
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.Stderr
	if errOut == nil {
		errOut = os.Stderr
	}

	noColor := opts.NoColor || os.Getenv("NO_COLOR") != ""
	ver := strings.TrimSpace(opts.Version)
	if ver == "" {
		ver = DefaultVersion
	}

	reader := bufio.NewReader(in)
	state := &sessionState{
		rt:      rt,
		stdin:   in,
		stdout:  out,
		stderr:  errOut,
		reader:  reader,
		noColor: noColor,
		version: ver,
	}

	// Share our buffered Stdin reader with the permission Gate when running in a real terminal
	// without an existing custom PromptFunc, preventing double-buffering on os.Stdin.
	if rt.Gate != nil && rt.Gate.PromptFunc == nil && isCharDeviceReader(in) {
		rt.Gate.SetPromptFunc(state.terminalApprovalPrompt)
	}

	state.printBanner()

	// Set up Ctrl+C handling so os.Interrupt cancels ONLY the active turn's context,
	// keeping the REPL loop and warm Copilot/HTTP client alive.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	doneCh := make(chan struct{})
	defer close(doneCh)

	var cancelMu sync.Mutex
	var activeTurnCancel context.CancelFunc

	handleInterrupt := func() {
		cancelMu.Lock()
		fn := activeTurnCancel
		cancelMu.Unlock()
		if fn != nil {
			fn()
		}
	}

	go func() {
		for {
			if opts.InterruptCh != nil {
				select {
				case <-doneCh:
					return
				case <-sigCh:
					handleInterrupt()
				case _, ok := <-opts.InterruptCh:
					if !ok {
						opts.InterruptCh = nil
						continue
					}
					handleInterrupt()
				}
			} else {
				select {
				case <-doneCh:
					return
				case <-sigCh:
					handleInterrupt()
				}
			}
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		promptText, eof, err := state.readPrompt()
		if err != nil {
			return err
		}
		trimmed := strings.TrimSpace(promptText)
		if trimmed == "" {
			if eof {
				return nil
			}
			continue
		}

		if strings.HasPrefix(trimmed, "/") {
			exitREPL, cmdErr := state.handleSlashCommand(ctx, trimmed)
			if cmdErr != nil {
				state.printfErr("[error] %v\n", cmdErr)
			}
			if exitREPL || eof {
				return nil
			}
			continue
		}

		turnInput := promptText
		if state.queuedSkill != "" {
			turnInput = state.queuedSkill + "\n\n" + promptText
			state.queuedSkill = ""
		}

		turnCtx, cancelTurn := context.WithCancel(ctx)
		cancelMu.Lock()
		activeTurnCancel = cancelTurn
		cancelMu.Unlock()

		_, _, turnErr := StreamTurn(turnCtx, rt.Agent, turnInput, out, errOut, false, noColor)

		cancelMu.Lock()
		activeTurnCancel = nil
		cancelMu.Unlock()
		cancelTurn()

		if turnErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(turnErr, context.Canceled) {
				state.printfErr("\n[interrupted]\n")
			} else if copilot.IsAuthError(turnErr) {
				canPrompt := isCharDeviceReader(in)
				if opts.AllowAuthPrompt != nil {
					canPrompt = *opts.AllowAuthPrompt
				}
				if canPrompt && state.recoverFromCopilotAuthError(ctx) {
					retryCtx, cancelRetry := context.WithCancel(ctx)
					cancelMu.Lock()
					activeTurnCancel = cancelRetry
					cancelMu.Unlock()

					_, _, retryErr := StreamTurn(retryCtx, rt.Agent, turnInput, out, errOut, false, noColor)

					cancelMu.Lock()
					activeTurnCancel = nil
					cancelMu.Unlock()
					cancelRetry()

					if retryErr != nil && !errors.Is(retryErr, context.Canceled) {
						state.printfErr("[error] %v\n", retryErr)
					}
				} else if !canPrompt {
					state.printfErr("[error] %v\n", turnErr)
				}
			} else {
				state.printfErr("[error] %v\n", turnErr)
			}
		}

		if eof {
			return nil
		}
	}
}

func (s *sessionState) printBanner() {
	provName := s.rt.Agent.ProviderName()
	modelName := s.rt.Agent.Model()
	if strings.TrimSpace(modelName) == "" {
		modelName = "auto"
	}
	policy := types.ApprovalSmartAuto
	if s.rt.Gate != nil {
		policy = s.rt.Gate.Policy()
	}
	compatState := "off"
	if s.rt.Config != nil && (s.rt.Config.Compatibility.LoadCopilotInstructions || s.rt.Config.Compatibility.LoadCopilotSkills) {
		compatState = "on"
	}

	globalCount, scopedActiveCount, scopedTotalCount, skillCount, customToolCount := s.customizationCounts()

	header := fmt.Sprintf("coder v%s", s.version)
	if !s.noColor {
		header = ansiBold + ansiCyan + header + ansiReset
	}

	_, _ = fmt.Fprintf(
		s.stdout,
		"%s | workspace: %s\nprovider: %s/%s | mode: %s | copilot-compat: %s\ncustomizations: %d global + %d/%d scoped instructions | %d skills | %d custom tools\n",
		header,
		s.rt.WorkspaceDir,
		provName,
		modelName,
		policy,
		compatState,
		globalCount,
		scopedActiveCount,
		scopedTotalCount,
		skillCount,
		customToolCount,
	)
	flushWriter(s.stdout)
}

func (s *sessionState) customizationCounts() (globalInst, activeScoped, totalScoped, skills, customTools int) {
	if s.rt.Customizations == nil {
		return 0, 0, 0, 0, 0
	}
	if inst := s.rt.Customizations.Instructions; inst != nil {
		globalInst = len(inst.GlobalInstructions)
		totalScoped = len(inst.ScopedInstructions)
		if totalScoped > 0 && s.rt.Toolset != nil {
			wsFiles := s.rt.Toolset.WorkspaceFiles(1000)
			activeScoped = len(inst.ActiveScopedInstructions(wsFiles))
		}
	}
	if s.rt.Customizations.Skills != nil {
		skills = len(s.rt.Customizations.Skills.List())
	}
	customTools = len(s.rt.Customizations.CustomTools)
	return
}

// readPrompt reads a single- or multi-line user prompt supporting trailing '\' continuation
// and clean exit on EOF (Ctrl+D) or Windows Ctrl+Z (0x1A).
func (s *sessionState) readPrompt() (string, bool, error) {
	var lines []string
	for {
		promptLabel := "coder> "
		if len(lines) > 0 {
			promptLabel = "... > "
		}
		if !s.noColor {
			promptLabel = ansiBold + promptLabel + ansiReset
		}
		_, _ = io.WriteString(s.stdout, promptLabel)
		flushWriter(s.stdout)

		raw, err := s.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}

		isEOF := errors.Is(err, io.EOF)
		line := strings.TrimRight(raw, "\r\n")

		// Handle Ctrl+Z (ASCII SUB 0x1A) on Windows consoles.
		if idx := strings.IndexByte(line, 0x1a); idx >= 0 {
			line = line[:idx]
			isEOF = true
		}

		if isEOF && line == "" && len(lines) == 0 {
			_, _ = io.WriteString(s.stdout, "\n")
			flushWriter(s.stdout)
			return "", true, nil
		}

		if strings.HasSuffix(line, `\`) && !isEOF {
			lines = append(lines, strings.TrimSuffix(line, `\`))
			continue
		}

		lines = append(lines, line)
		return strings.Join(lines, "\n"), isEOF, nil
	}
}

func (s *sessionState) terminalApprovalPrompt(ctx context.Context, req types.ApprovalRequest) (bool, error) {
	detail := strings.TrimSpace(req.Summary)
	if detail == "" {
		switch {
		case req.Command != "":
			detail = req.Command
		case req.TargetPath != "":
			detail = req.TargetPath
		default:
			detail = req.ToolName
		}
	}
	_, _ = fmt.Fprintf(s.stderr, "\n[permission] %s (%s): %s — Allow? [y/N/a(lways)]: ", req.ToolName, req.Category, detail)
	flushWriter(s.stderr)

	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := s.reader.ReadString('\n')
		ch <- res{line: l, err: err}
	}()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case r := <-ch:
		if r.err != nil && !errors.Is(r.err, io.EOF) {
			return false, r.err
		}
		ans := strings.ToLower(strings.TrimSpace(r.line))
		switch ans {
		case "y", "yes":
			return true, nil
		case "a", "always":
			if s.rt.Gate != nil {
				s.rt.Gate.SetPolicy(types.ApprovalFullAuto)
			}
			return true, nil
		default:
			return false, nil
		}
	}
}

func (s *sessionState) printfOut(format string, args ...any) {
	_, _ = fmt.Fprintf(s.stdout, format, args...)
	flushWriter(s.stdout)
}

func (s *sessionState) printfErr(format string, args ...any) {
	_, _ = fmt.Fprintf(s.stderr, format, args...)
	flushWriter(s.stderr)
}

func (s *sessionState) detectReachableLocalProvider() (name string, url string) {
	type candidate struct {
		name     string
		probeURL string
		dispURL  string
	}
	candidates := []candidate{
		{name: "lmstudio", probeURL: "http://127.0.0.1:1234/v1/models", dispURL: "http://127.0.0.1:1234/v1"},
		{name: "ollama", probeURL: "http://127.0.0.1:11434/api/tags", dispURL: "http://127.0.0.1:11434/v1"},
		{name: "local", probeURL: "http://127.0.0.1:8080/v1/models", dispURL: "http://127.0.0.1:8080/v1"},
	}
	client := &http.Client{Timeout: 250 * time.Millisecond}
	for _, c := range candidates {
		if s.rt.Config != nil {
			if _, ok := s.rt.Config.Providers[c.name]; !ok {
				continue
			}
		}
		resp, err := client.Get(c.probeURL)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return c.name, c.dispURL
		}
	}
	return "", ""
}

func (s *sessionState) recoverFromCopilotAuthError(ctx context.Context) bool {
	localName, localURL := s.detectReachableLocalProvider()

	s.printfErr("GitHub Copilot CLI is not signed in yet.\n")
	s.printfErr("  [1] Sign in now with GitHub OAuth (`copilot login`)\n")
	if localName != "" {
		s.printfErr("  [2] Switch to detected local model (`%s` at %s)\n", localName, localURL)
	}
	s.printfErr("  [n] Cancel (or type /login, /provider <name>, or set COPILOT_GITHUB_TOKEN)\n")
	s.printfErr("Select [1]: ")

	type lineRes struct {
		line string
		err  error
	}
	ch := make(chan lineRes, 1)
	go func() {
		l, err := s.reader.ReadString('\n')
		ch <- lineRes{line: l, err: err}
	}()

	select {
	case <-ctx.Done():
		return false
	case r := <-ch:
		if r.err != nil && !errors.Is(r.err, io.EOF) {
			return false
		}
		choice := strings.ToLower(strings.TrimSpace(r.line))
		switch {
		case choice == "" || choice == "1" || choice == "y" || choice == "yes" || choice == "login":
			if err := s.cmdLogin(ctx, nil); err != nil {
				s.printfErr("[error] %v\n", err)
				return false
			}
			return true
		case choice == "2" && localName != "":
			if err := s.cmdProvider(ctx, []string{localName}); err != nil {
				s.printfErr("[error] %v\n", err)
				return false
			}
			return true
		default:
			return false
		}
	}
}

// StreamTurn executes a single turn on ag while streaming text deltas to stdout
// with zero buffering lag and writing unobtrusive tool & telemetry lines to stderr.
func StreamTurn(
	ctx context.Context,
	ag *agent.Agent,
	prompt string,
	stdout io.Writer,
	stderr io.Writer,
	quietStderr bool,
	noColor bool,
) (string, types.TurnMetrics, error) {
	var mu sync.Mutex
	streamedText := false
	endsWithNewline := true

	onEvent := func(ev types.StreamEvent) {
		mu.Lock()
		defer mu.Unlock()

		switch ev.Type {
		case types.EventTextDelta:
			if ev.Text == "" || stdout == nil {
				return
			}
			_, _ = io.WriteString(stdout, ev.Text)
			flushWriter(stdout)
			streamedText = true
			endsWithNewline = strings.HasSuffix(ev.Text, "\n")

		case types.EventToolCallStart:
			if quietStderr || stderr == nil {
				return
			}
			if stdout != nil && stdout == stderr && streamedText && !endsWithNewline {
				_, _ = io.WriteString(stdout, "\n")
				endsWithNewline = true
			}
			line := FormatToolCallStart(ev.ToolName, ev.ToolArgs)
			if !noColor {
				line = ansiDim + line + ansiReset
			}
			_, _ = io.WriteString(stderr, line+"\n")
			flushWriter(stderr)

		case types.EventToolCallEnd:
			if quietStderr || stderr == nil {
				return
			}
			line := FormatToolCallEnd(ev.ToolName, ev.ToolResult)
			if !noColor {
				if ev.ToolResult != nil && ev.ToolResult.IsError {
					line = ansiRed + line + ansiReset
				} else {
					line = ansiDim + line + ansiReset
				}
			}
			_, _ = io.WriteString(stderr, line+"\n")
			flushWriter(stderr)
		}
	}

	finalText, metrics, err := ag.RunTurn(ctx, prompt, onEvent)

	mu.Lock()
	defer mu.Unlock()

	if !streamedText && finalText != "" && stdout != nil {
		_, _ = io.WriteString(stdout, finalText)
		streamedText = true
		endsWithNewline = strings.HasSuffix(finalText, "\n")
	}
	if streamedText && !endsWithNewline && stdout != nil {
		_, _ = io.WriteString(stdout, "\n")
	}
	if stdout != nil {
		flushWriter(stdout)
	}

	if metrics.Provider == "" {
		metrics.Provider = ag.ProviderName()
	}
	if metrics.Model == "" {
		metrics.Model = ag.Model()
	}

	if err == nil && !quietStderr && stderr != nil {
		summary := FormatTurnMetrics(metrics)
		if !noColor {
			summary = ansiDim + summary + ansiReset
		}
		_, _ = io.WriteString(stderr, summary+"\n")
		flushWriter(stderr)
	}

	return finalText, metrics, err
}

// FormatToolCallStart renders a compact 1-line summary truncated cleanly to ~100 characters.
func FormatToolCallStart(toolName string, rawArgs string) string {
	compactArgs := strings.Join(strings.Fields(strings.TrimSpace(rawArgs)), " ")
	if compactArgs == "{}" {
		compactArgs = ""
	}
	line := "  -> " + toolName
	if compactArgs != "" {
		line += " " + compactArgs
	}
	return truncateSingleLine(line, 100)
}

// FormatToolCallEnd renders a compact 1-line tool completion status with duration.
func FormatToolCallEnd(toolName string, res *types.ToolResult) string {
	status := "ok"
	var durMs int64
	if res != nil {
		if res.IsError {
			status = "error"
		}
		durMs = res.DurationMs
	}
	return fmt.Sprintf("  <- %s %s (%dms)", toolName, status, durMs)
}

// FormatTurnMetrics renders the post-turn latency and usage telemetry line:
// [ttft: 240ms | total: 1.42s | llm: 2 | tools: 3 | provider: copilot/claude-sonnet-4.5]
func FormatTurnMetrics(m types.TurnMetrics) string {
	provModel := m.Provider
	if m.Model != "" {
		if provModel != "" {
			provModel = provModel + "/" + m.Model
		} else {
			provModel = m.Model
		}
	}
	if provModel == "" {
		provModel = "unknown"
	}
	return fmt.Sprintf(
		"[ttft: %s | total: %s | llm: %d | tools: %d | provider: %s]",
		formatCompactDuration(m.TTFT),
		formatCompactDuration(m.TotalDuration),
		m.LLMCalls,
		m.ToolCalls,
		provModel,
	)
}

func formatCompactDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func truncateSingleLine(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func flushWriter(w io.Writer) {
	if w == nil {
		return
	}
	if flusher, ok := w.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
}

func isCharDeviceReader(r io.Reader) bool {
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
