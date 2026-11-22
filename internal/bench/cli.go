package bench

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

type optionalBoolFlag struct {
	ptr **bool
}

func (f optionalBoolFlag) String() string {
	if f.ptr == nil || *f.ptr == nil {
		return ""
	}
	return strconv.FormatBool(**f.ptr)
}

func (f optionalBoolFlag) Set(s string) error {
	val, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid boolean value %q", s)
	}
	*f.ptr = &val
	return nil
}

func (f optionalBoolFlag) IsBoolFlag() bool { return true }

type inverseBoolFlag struct {
	ptr **bool
}

func (f inverseBoolFlag) String() string {
	if f.ptr == nil || *f.ptr == nil {
		return "false"
	}
	return strconv.FormatBool(!**f.ptr)
}

func (f inverseBoolFlag) Set(s string) error {
	val, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid boolean value %q", s)
	}
	inv := !val
	*f.ptr = &inv
	return nil
}

func (f inverseBoolFlag) IsBoolFlag() bool { return true }

type multiStringFlag []string

func (m *multiStringFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiStringFlag) Set(val string) error {
	*m = append(*m, val)
	return nil
}

// RunCLI parses `coder bench` command-line flags, executes listing or the benchmark suite,
// renders the ASCII scorecard (or JSON report), and returns exit code 0 on all-pass or 1 otherwise.
func RunCLI(args []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	var (
		problemFlags    multiStringFlag
		lang            string
		providerName    string
		modelName       string
		baseURL         string
		apiKey          string
		copilotCompat   *bool
		problemsDir     string
		timeoutSec      int
		maxSteps        int
		reasoningEffort string
		keepWorkDirs    bool
		verbose         bool
		jsonOut         bool
		listOnly        bool
		showHelp        bool
	)

	fs := flag.NewFlagSet("coder bench", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.Var(&problemFlags, "problems", "Comma-separated benchmark problem IDs to run (default: all)")
	fs.StringVar(&lang, "lang", "", "Filter problems by language: cpp or python")
	fs.StringVar(&providerName, "provider", "", "Model provider name (copilot, ollama, local, openai, openrouter, anthropic)")
	fs.StringVar(&providerName, "p", "", "Model provider name (shorthand)")
	fs.StringVar(&modelName, "model", "", "Model ID override")
	fs.StringVar(&modelName, "m", "", "Model ID override (shorthand)")
	fs.StringVar(&baseURL, "base-url", "", "Provider base URL override")
	fs.StringVar(&apiKey, "api-key", "", "Provider API key override")
	fs.Var(optionalBoolFlag{ptr: &copilotCompat}, "copilot-compat", "Enable or disable GitHub Copilot instructions/skills compatibility (true/false)")
	fs.Var(inverseBoolFlag{ptr: &copilotCompat}, "no-copilot-compat", "Disable GitHub Copilot instructions/skills compatibility")
	fs.StringVar(&problemsDir, "problems-dir", "", "Path to benchmark problems root directory (auto-detected if omitted)")
	fs.IntVar(&timeoutSec, "timeout", 0, "Per-problem timeout in seconds (overrides problem.json timeout_sec)")
	fs.IntVar(&maxSteps, "max-steps", 0, "Maximum agent tool-call steps per problem")
	fs.StringVar(&reasoningEffort, "reasoning-effort", "", "Optional reasoning effort level (low, medium, high)")
	fs.BoolVar(&keepWorkDirs, "keep-workdirs", false, "Preserve temporary benchmark workspaces on disk after verification")
	fs.BoolVar(&verbose, "verbose", false, "Stream live agent output and tool calls to stderr")
	fs.BoolVar(&verbose, "v", false, "Stream live agent output and tool calls to stderr (shorthand)")
	fs.BoolVar(&jsonOut, "json", false, "Emit structured JSON SuiteReport to stdout")
	fs.BoolVar(&listOnly, "list", false, "List available benchmark problems without running them")
	fs.BoolVar(&showHelp, "help", false, "Show help for coder bench")
	fs.BoolVar(&showHelp, "h", false, "Show help for coder bench (shorthand)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printBenchUsage(stdout)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "coder bench: %v\n\n", err)
		printBenchUsage(stderr)
		return 1
	}

	if showHelp {
		printBenchUsage(stdout)
		return 0
	}

	if rest := fs.Args(); len(rest) > 0 {
		_, _ = fmt.Fprintf(stderr, "coder bench: unexpected positional arguments: %s\n", strings.Join(rest, " "))
		return 1
	}

	filterIDs := []string(problemFlags)

	if listOnly {
		problems, err := LoadProblems(problemsDir, filterIDs)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "coder bench: %v\n", err)
			return 1
		}
		problems, err = FilterProblemsByLanguage(problems, lang)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "coder bench: %v\n", err)
			return 1
		}
		if jsonOut {
			enc, err := json.MarshalIndent(problems, "", "  ")
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "coder bench: encode JSON: %v\n", err)
				return 1
			}
			_, _ = fmt.Fprintln(stdout, string(enc))
			return 0
		}
		_, _ = io.WriteString(stdout, FormatProblemList(problems))
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	report, err := RunSuite(ctx, SuiteOptions{
		ProblemsRoot:    problemsDir,
		ProblemIDs:      filterIDs,
		Language:        lang,
		Provider:        providerName,
		Model:           modelName,
		BaseURL:         baseURL,
		APIKey:          apiKey,
		CopilotCompat:   copilotCompat,
		TimeoutSec:      timeoutSec,
		MaxSteps:        maxSteps,
		ReasoningEffort: reasoningEffort,
		KeepWorkDirs:    keepWorkDirs,
		Verbose:         verbose,
		Stderr:          stderr,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "coder bench: %v\n", err)
		return 1
	}

	if jsonOut {
		enc, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "coder bench: encode JSON report: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, string(enc))
	} else {
		_, _ = io.WriteString(stdout, FormatScorecard(report))
	}

	if report.TotalProblems > 0 && report.FailedProblems == 0 {
		return 0
	}
	return 1
}

// FormatProblemList renders an ASCII table of discovered benchmark problems.
func FormatProblemList(problems []Problem) string {
	headers := []string{"Problem ID", "Lang", "Timeout", "Copilot Compat", "Title"}
	rows := make([][]string, 0, len(problems))
	for _, p := range problems {
		compat := "no"
		if p.CopilotCompatRequired {
			compat = "required"
		}
		rows = append(rows, []string{
			p.ID,
			p.Language,
			fmt.Sprintf("%ds", p.TimeoutSec),
			compat,
			p.Title,
		})
	}
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "Available benchmark problems (%d):\n", len(problems))
	b.WriteString(renderASCIITable(headers, rows))
	return b.String()
}

// FormatScorecard renders a crisp ASCII summary scorecard table and pass/latency summary.
func FormatScorecard(report *SuiteReport) string {
	if report == nil {
		return "No benchmark results.\n"
	}

	var b strings.Builder
	headerMeta := ""
	if report.Provider != "" || report.Model != "" {
		headerMeta = fmt.Sprintf(" (provider: %s, model: %s)",
			firstNonEmpty(report.Provider, "default"),
			firstNonEmpty(report.Model, "default"),
		)
	}
	_, _ = fmt.Fprintf(&b, "coder benchmark scorecard%s\n", headerMeta)

	headers := []string{"Problem ID", "Lang", "Status", "TTFT", "Total Time", "LLM Calls", "Tool Calls"}
	rows := make([][]string, 0, len(report.Results))
	for _, r := range report.Results {
		rows = append(rows, []string{
			r.ProblemID,
			r.Language,
			r.Status,
			formatMsShort(r.TTFTMs),
			formatMsShort(r.TotalTimeMs),
			strconv.Itoa(r.LLMCalls),
			strconv.Itoa(r.ToolCalls),
		})
	}
	b.WriteString(renderASCIITable(headers, rows))

	_, _ = fmt.Fprintf(
		&b,
		"Summary: %d/%d passed (%.1f%%) | Avg TTFT: %s | Avg Latency: %s | Suite Time: %s\n",
		report.PassedProblems,
		report.TotalProblems,
		report.PassRatePct,
		formatMsShort(report.AvgTTFTMs),
		formatMsShort(report.AvgTotalTimeMs),
		formatMsShort(report.TotalTimeMs),
	)

	var hasFailures bool
	for _, r := range report.Results {
		if !r.Passed {
			if !hasFailures {
				b.WriteString("\nFailures:\n")
				hasFailures = true
			}
			_, _ = fmt.Fprintf(&b, "- %s: %s\n", r.ProblemID, firstNonEmpty(r.Error, "verification failed"))
			if snippet := trimFailureSnippet(r.VerifyOutput, 6, 400); snippet != "" {
				for _, line := range strings.Split(snippet, "\n") {
					_, _ = fmt.Fprintf(&b, "    %s\n", line)
				}
			}
		}
		if r.WorkspaceDir != "" {
			_, _ = fmt.Fprintf(&b, "  [workdir] %s: %s\n", r.ProblemID, r.WorkspaceDir)
		}
	}

	return b.String()
}

func renderASCIITable(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i := 0; i < len(headers) && i < len(row); i++ {
			if len(row[i]) > widths[i] {
				widths[i] = len(row[i])
			}
		}
	}

	var sep strings.Builder
	sep.WriteByte('+')
	for _, w := range widths {
		sep.WriteString(strings.Repeat("-", w+2))
		sep.WriteByte('+')
	}
	sepLine := sep.String() + "\n"

	writeRow := func(cols []string) string {
		var r strings.Builder
		r.WriteByte('|')
		for i, w := range widths {
			val := ""
			if i < len(cols) {
				val = cols[i]
			}
			_, _ = fmt.Fprintf(&r, " %-*s |", w, val)
		}
		r.WriteByte('\n')
		return r.String()
	}

	var out strings.Builder
	out.WriteString(sepLine)
	out.WriteString(writeRow(headers))
	out.WriteString(sepLine)
	for _, row := range rows {
		out.WriteString(writeRow(row))
	}
	out.WriteString(sepLine)
	return out.String()
}

func formatDurationShort(d time.Duration) string {
	return formatMsShort(d.Milliseconds())
}

func formatMsShort(ms int64) string {
	if ms <= 0 {
		return "0ms"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000.0)
}

func trimFailureSnippet(output string, maxLines int, maxChars int) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > maxChars {
		trimmed = trimmed[:maxChars] + "..."
	}
	lines := strings.Split(strings.ReplaceAll(trimmed, "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "...")
	}
	return strings.Join(lines, "\n")
}

func printBenchUsage(w io.Writer) {
	_, _ = io.WriteString(w, `Usage: coder bench [flags]

Run coder's automated C++20 and Python coding benchmark suite in isolated sandboxes
with tamper-proof host verification.

Flags:
  --list                        List available benchmark problems without running
  --problems <id1,id2>          Comma-separated problem IDs to run (default: all)
  --lang <cpp|python>           Filter benchmark problems by language
  -p, --provider <name>         Provider profile (copilot, ollama, local, openai, openrouter, anthropic)
  -m, --model <id>              Model ID override
  --base-url <url>              Provider endpoint base URL override
  --api-key <key>               Provider API key override
  --copilot-compat=<bool>       Enable or disable GitHub Copilot instructions/skills compatibility
  --no-copilot-compat           Disable GitHub Copilot instructions/skills compatibility
  --problems-dir <dir>          Override path to benchmark problems root
  --timeout <sec>               Per-problem timeout in seconds
  --max-steps <n>               Maximum agent tool-call steps per problem
  --reasoning-effort <level>    Reasoning effort override (low, medium, high)
  --keep-workdirs               Keep temporary benchmark workspaces on disk
  -v, --verbose                 Stream live agent tool calls and output to stderr
  --json                        Emit structured JSON SuiteReport to stdout
  -h, --help                    Show this help message
`)
}
