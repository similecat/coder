package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"coder/internal/bootstrap"
	"coder/internal/config"
	"coder/internal/types"
)

// SuiteOptions configures a benchmark suite or single-problem run.
type SuiteOptions struct {
	ProblemsRoot    string
	ProblemIDs      []string
	Language        string
	Provider        string
	Model           string
	BaseURL         string
	APIKey          string
	CopilotCompat   *bool
	TimeoutSec      int
	MaxSteps        int
	ReasoningEffort string
	KeepWorkDirs    bool
	Verbose         bool
	UserHome        string
	CustomProvider  types.Provider
	Stderr          io.Writer
}

// ProblemResult captures agent metrics and host verification results for one benchmark problem.
type ProblemResult struct {
	ProblemID      string        `json:"problem_id"`
	Title          string        `json:"title"`
	Language       string        `json:"language"`
	Passed         bool          `json:"passed"`
	Status         string        `json:"status"`
	Provider       string        `json:"provider"`
	Model          string        `json:"model"`
	TTFTMs         int64         `json:"ttft_ms"`
	TotalTimeMs    int64         `json:"total_time_ms"`
	AgentTimeMs    int64         `json:"agent_time_ms"`
	VerifyTimeMs   int64         `json:"verify_time_ms"`
	LLMCalls       int           `json:"llm_calls"`
	ToolCalls      int           `json:"tool_calls"`
	InputTokens    int           `json:"input_tokens"`
	OutputTokens   int           `json:"output_tokens"`
	CachedTokens   int           `json:"cached_tokens"`
	WorkspaceDir   string        `json:"workspace_dir,omitempty"`
	Compiler       string        `json:"compiler,omitempty"`
	VerifyExitCode int           `json:"verify_exit_code"`
	VerifyOutput   string        `json:"verify_output,omitempty"`
	AgentResponse  string        `json:"agent_response,omitempty"`
	Error          string        `json:"error,omitempty"`
	TTFT           time.Duration `json:"-"`
	TotalDuration  time.Duration `json:"-"`
}

// SuiteReport summarizes an entire benchmark suite execution.
type SuiteReport struct {
	Provider       string          `json:"provider,omitempty"`
	Model          string          `json:"model,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     time.Time       `json:"finished_at"`
	TotalTimeMs    int64           `json:"total_time_ms"`
	TotalProblems  int             `json:"total_problems"`
	PassedProblems int             `json:"passed_problems"`
	FailedProblems int             `json:"failed_problems"`
	PassRatePct    float64         `json:"pass_rate_pct"`
	AvgTTFTMs      int64           `json:"avg_ttft_ms"`
	AvgTotalTimeMs int64           `json:"avg_total_time_ms"`
	Results        []ProblemResult `json:"results"`
}

// RunSuite loads matching problems, executes each in an isolated temporary sandbox,
// verifies results via tamper-proof host harnesses, and compiles a SuiteReport.
func RunSuite(ctx context.Context, opts SuiteOptions) (*SuiteReport, error) {
	problems, err := LoadProblems(opts.ProblemsRoot, opts.ProblemIDs)
	if err != nil {
		return nil, err
	}
	problems, err = FilterProblemsByLanguage(problems, opts.Language)
	if err != nil {
		return nil, err
	}
	if len(problems) == 0 {
		return nil, fmt.Errorf("no benchmark problems matched filter (lang=%q, problems=%v)", opts.Language, opts.ProblemIDs)
	}

	startedAt := time.Now()
	results := make([]ProblemResult, 0, len(problems))

	var (
		passedCount int
		sumTTFTMs   int64
		sumTotalMs  int64
		suiteProv   string
		suiteModel  string
	)

	for idx, prob := range problems {
		if ctx.Err() != nil {
			break
		}
		if opts.Verbose && opts.Stderr != nil {
			_, _ = fmt.Fprintf(
				opts.Stderr,
				"\n=== [%d/%d] %s (%s) — %s ===\n",
				idx+1,
				len(problems),
				prob.ID,
				prob.Language,
				prob.Title,
			)
		}

		res := RunProblem(ctx, prob, opts)
		results = append(results, res)

		if suiteProv == "" && res.Provider != "" {
			suiteProv = res.Provider
		}
		if suiteModel == "" && res.Model != "" {
			suiteModel = res.Model
		}
		if res.Passed {
			passedCount++
		}
		sumTTFTMs += res.TTFTMs
		sumTotalMs += res.TotalTimeMs

		if opts.Verbose && opts.Stderr != nil {
			_, _ = fmt.Fprintf(
				opts.Stderr,
				"\n--- [%s] %s | ttft=%s | agent=%dms | verify=%dms | total=%s | llm=%d | tools=%d ---\n",
				res.Status,
				prob.ID,
				formatDurationShort(res.TTFT),
				res.AgentTimeMs,
				res.VerifyTimeMs,
				formatDurationShort(res.TotalDuration),
				res.LLMCalls,
				res.ToolCalls,
			)
			if !res.Passed && res.Error != "" {
				_, _ = fmt.Fprintf(opts.Stderr, "    reason: %s\n", res.Error)
			}
		}
	}

	finishedAt := time.Now()
	totalCount := len(results)
	failedCount := totalCount - passedCount

	var passRate float64
	var avgTTFTMs, avgTotalMs int64
	if totalCount > 0 {
		passRate = (float64(passedCount) * 100.0) / float64(totalCount)
		avgTTFTMs = sumTTFTMs / int64(totalCount)
		avgTotalMs = sumTotalMs / int64(totalCount)
	}

	return &SuiteReport{
		Provider:       suiteProv,
		Model:          suiteModel,
		StartedAt:      startedAt,
		FinishedAt:     finishedAt,
		TotalTimeMs:    finishedAt.Sub(startedAt).Milliseconds(),
		TotalProblems:  totalCount,
		PassedProblems: passedCount,
		FailedProblems: failedCount,
		PassRatePct:    passRate,
		AvgTTFTMs:      avgTTFTMs,
		AvgTotalTimeMs: avgTotalMs,
		Results:        results,
	}, nil
}

// RunProblem executes a single benchmark problem inside a fresh temporary workspace
// and runs the host verifier after closing the agent runtime.
func RunProblem(ctx context.Context, prob Problem, opts SuiteOptions) ProblemResult {
	problemStart := time.Now()

	res := ProblemResult{
		ProblemID:      prob.ID,
		Title:          prob.Title,
		Language:       prob.Language,
		Status:         "FAIL",
		VerifyExitCode: -1,
	}

	finalize := func() ProblemResult {
		res.TotalDuration = time.Since(problemStart)
		res.TotalTimeMs = res.TotalDuration.Milliseconds()
		if res.Passed {
			res.Status = "PASS"
		} else {
			res.Status = "FAIL"
		}
		return res
	}

	tempWorkspaceDir, err := os.MkdirTemp("", "coder-bench-"+prob.ID+"-")
	if err != nil {
		res.Error = fmt.Sprintf("create temp workspace: %v", err)
		return finalize()
	}
	if opts.KeepWorkDirs {
		res.WorkspaceDir = tempWorkspaceDir
	} else {
		defer func() { _ = os.RemoveAll(tempWorkspaceDir) }()
	}

	if err := CopyWorkspace(prob.WorkspaceDir, tempWorkspaceDir); err != nil {
		res.Error = fmt.Sprintf("copy starter workspace: %v", err)
		return finalize()
	}

	interactive := false
	rt, err := bootstrap.Build(bootstrap.Options{
		WorkspaceDir: tempWorkspaceDir,
		UserHome:     opts.UserHome,
		Overrides: config.Overrides{
			Provider:        opts.Provider,
			Model:           opts.Model,
			ApprovalPolicy:  types.ApprovalFullAuto,
			FullAuto:        true,
			CopilotCompat:   opts.CopilotCompat,
			BaseURL:         opts.BaseURL,
			APIKey:          opts.APIKey,
			MaxSteps:        opts.MaxSteps,
			ReasoningEffort: opts.ReasoningEffort,
		},
		CustomProvider: opts.CustomProvider,
		Interactive:    &interactive,
		Stderr:         opts.Stderr,
		WarmToolchains: prob.Language == "cpp",
	})
	if err != nil {
		res.Error = fmt.Sprintf("bootstrap runtime: %v", err)
		return finalize()
	}

	res.Provider = rt.Agent.ProviderName()
	res.Model = rt.Agent.Model()

	if prob.CopilotCompatRequired && rt.Config != nil &&
		(!rt.Config.Compatibility.LoadCopilotInstructions || !rt.Config.Compatibility.LoadCopilotSkills) &&
		opts.Verbose && opts.Stderr != nil {
		_, _ = fmt.Fprintf(opts.Stderr, "  [warn] %s requires Copilot compatibility, which is currently disabled\n", prob.ID)
	}

	timeoutSec := prob.TimeoutSec
	if opts.TimeoutSec > 0 {
		timeoutSec = opts.TimeoutSec
	}
	if timeoutSec <= 0 {
		timeoutSec = defaultProblemTimeoutSec
	}

	turnCtx, cancelTurn := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	onEvent := buildVerboseEventLogger(opts.Verbose, opts.Stderr)

	agentStart := time.Now()
	finalText, metrics, turnErr := rt.Agent.RunTurn(turnCtx, prob.Prompt, onEvent)
	res.AgentTimeMs = time.Since(agentStart).Milliseconds()
	cancelTurn()

	// Always close runtime before running verification and cleaning up the temp workspace.
	_ = rt.Close()

	if metrics.Provider != "" {
		res.Provider = metrics.Provider
	}
	if metrics.Model != "" {
		res.Model = metrics.Model
	}
	res.TTFT = metrics.TTFT
	res.TTFTMs = metrics.TTFT.Milliseconds()
	res.LLMCalls = metrics.LLMCalls
	res.ToolCalls = metrics.ToolCalls
	res.InputTokens = metrics.InputTokens
	res.OutputTokens = metrics.OutputTokens
	res.CachedTokens = metrics.CachedTokens
	res.AgentResponse = strings.TrimSpace(finalText)

	if ctx.Err() != nil {
		res.Error = fmt.Sprintf("benchmark interrupted: %v", ctx.Err())
		return finalize()
	}

	verifyRes := VerifyWorkspace(ctx, prob, tempWorkspaceDir)
	res.VerifyTimeMs = verifyRes.DurationMs
	res.Compiler = verifyRes.Compiler
	res.VerifyExitCode = verifyRes.ExitCode
	res.VerifyOutput = verifyRes.Output

	if turnErr != nil && !verifyRes.Passed {
		res.Error = fmt.Sprintf("agent turn error: %v; verify failed: %s", turnErr, firstNonEmpty(verifyRes.Error, "non-zero exit"))
		return finalize()
	}
	if turnErr != nil {
		res.Error = fmt.Sprintf("agent turn error: %v", turnErr)
		return finalize()
	}
	if !verifyRes.Passed {
		res.Error = firstNonEmpty(verifyRes.Error, "host verification failed")
		return finalize()
	}

	res.Passed = true
	return finalize()
}

func buildVerboseEventLogger(verbose bool, stderr io.Writer) func(types.StreamEvent) {
	if !verbose || stderr == nil {
		return nil
	}
	var mu sync.Mutex
	inTextLine := false

	return func(ev types.StreamEvent) {
		mu.Lock()
		defer mu.Unlock()

		switch ev.Type {
		case types.EventTextDelta:
			if ev.Text != "" {
				_, _ = io.WriteString(stderr, ev.Text)
				inTextLine = !strings.HasSuffix(ev.Text, "\n")
			}
		case types.EventToolCallStart:
			if inTextLine {
				_, _ = io.WriteString(stderr, "\n")
				inTextLine = false
			}
			_, _ = fmt.Fprintf(stderr, "  -> [tool] %s(%s)\n", ev.ToolName, previewOneLine(ev.ToolArgs, 140))
		case types.EventToolCallEnd:
			status := "ok"
			var durMs int64
			if ev.ToolResult != nil {
				durMs = ev.ToolResult.DurationMs
				if ev.ToolResult.IsError {
					status = "error"
				}
			}
			_, _ = fmt.Fprintf(stderr, "  <- [tool] %s %s (%dms)\n", ev.ToolName, status, durMs)
		case types.EventError:
			if ev.Err != nil {
				_, _ = fmt.Fprintf(stderr, "\n  [error] %v\n", ev.Err)
				inTextLine = false
			}
		}
	}
}

func previewOneLine(s string, maxLen int) string {
	collapsed := strings.Join(strings.Fields(s), " ")
	if len(collapsed) <= maxLen {
		return collapsed
	}
	if maxLen <= 3 {
		return collapsed[:maxLen]
	}
	return collapsed[:maxLen-3] + "..."
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
