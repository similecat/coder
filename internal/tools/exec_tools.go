package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"coder/internal/types"
)

const (
	defaultCmdTimeoutSec = 120
	maxCmdTimeoutSec     = 600
)

// RunCommandTool executes shell commands with timeout cancellation, MSVC environment acceleration,
// and head+tail bounded output capture.
type RunCommandTool struct {
	workspaceDir   string
	approver       types.Approver
	maxOutputBytes int
}

// NewRunCommandTool constructs the run_command tool.
func NewRunCommandTool(workspaceDir string, approver types.Approver) types.Tool {
	return &RunCommandTool{
		workspaceDir:   normalizeWorkspaceDir(workspaceDir),
		approver:       approver,
		maxOutputBytes: defaultMaxOutputBytes,
	}
}

func newRunCommandToolWithCap(workspaceDir string, approver types.Approver, maxOutputBytes int) types.Tool {
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	return &RunCommandTool{
		workspaceDir:   normalizeWorkspaceDir(workspaceDir),
		approver:       approver,
		maxOutputBytes: maxOutputBytes,
	}
}

func (t *RunCommandTool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "run_command",
		Description: "Execute a shell command (PowerShell on Windows, /bin/sh on Unix) with automatic MSVC C++ build environment setup, timeout enforcement, and bounded head+tail output.",
		Category:    types.ToolCategoryExecute,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command string to execute.",
				},
				"working_dir": map[string]any{
					"type":        "string",
					"description": "Optional working directory (relative to workspace root or absolute; defaults to workspace root).",
				},
				"timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Optional timeout in seconds (default: 120, max: 600).",
				},
			},
			"required": []string{"command"},
		},
	}
}

type runCommandArgs struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir"`
	TimeoutSec int    `json:"timeout_sec"`
}

func (t *RunCommandTool) Execute(ctx context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	var args runCommandArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("invalid run_command arguments: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	command := strings.TrimSpace(args.Command)
	if command == "" {
		return types.ToolResult{
			Content:    "run_command error: 'command' must not be empty",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	workDir := t.workspaceDir
	if strings.TrimSpace(args.WorkingDir) != "" {
		resolved, err := resolveToolPath(t.workspaceDir, args.WorkingDir)
		if err != nil {
			return types.ToolResult{
				Content:    fmt.Sprintf("run_command working_dir error: %v", err),
				IsError:    true,
				DurationMs: time.Since(start).Milliseconds(),
			}, nil
		}
		workDir = resolved
	}
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		return types.ToolResult{
			Content:    fmt.Sprintf("run_command error: working_dir does not exist or is not a directory: %s", workDir),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	approved, reason, err := checkApproval(ctx, t.approver, types.ApprovalRequest{
		ToolName:   "run_command",
		Category:   types.ToolCategoryExecute,
		Summary:    fmt.Sprintf("Run command: %s", summarizeText(command, 120)),
		Command:    command,
		TargetPath: workDir,
		WorkingDir: workDir,
	})
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("run_command permission error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if !approved {
		return types.ToolResult{
			Content:    fmt.Sprintf("run_command permission denied (%s): %s", reason, summarizeText(command, 120)),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	timeoutSec := args.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultCmdTimeoutSec
	}
	if timeoutSec > maxCmdTimeoutSec {
		timeoutSec = maxCmdTimeoutSec
	}

	execStart := time.Now()
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := buildShellCommand(cmdCtx, command)
	cmd.Dir = workDir
	cmd.Env = ResolveMSVCEnv()

	outBuf := newHeadTailBuffer(t.maxOutputBytes)
	cmd.Stdout = outBuf
	cmd.Stderr = outBuf

	runErr := cmd.Run()
	durationMs := time.Since(execStart).Milliseconds()
	totalMs := time.Since(start).Milliseconds()

	timedOut := errors.Is(cmdCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	exitCode := extractExitCode(runErr, timedOut)

	output := strings.TrimRight(outBuf.String(), "\r\n")
	var content strings.Builder
	if output != "" {
		content.WriteString(output)
		content.WriteByte('\n')
	}
	if timedOut {
		_, _ = fmt.Fprintf(&content, "[timed out after %ds | exit_code=%d | duration_ms=%d]", timeoutSec, exitCode, durationMs)
	} else {
		_, _ = fmt.Fprintf(&content, "[exit_code=%d | duration_ms=%d]", exitCode, durationMs)
	}

	return types.ToolResult{
		Content:    content.String(),
		IsError:    exitCode != 0 || timedOut || runErr != nil,
		DurationMs: totalMs,
		Metadata: map[string]any{
			"exit_code":       exitCode,
			"duration_ms":     durationMs,
			"timed_out":       timedOut,
			"truncated":       outBuf.Truncated(),
			"total_out_bytes": outBuf.TotalBytes(),
		},
	}, nil
}

func extractExitCode(runErr error, timedOut bool) int {
	if runErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code := exitErr.ExitCode()
		if code != 0 {
			return code
		}
	}
	if timedOut {
		return -1
	}
	return 1
}

func summarizeText(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// --- LoadSkillTool ---

// LoadSkillTool loads on-demand SKILL.md instructions and helper file listings from a SkillCatalog.
type LoadSkillTool struct {
	catalog types.SkillCatalog
}

// NewLoadSkillTool constructs the load_skill tool.
func NewLoadSkillTool(catalog types.SkillCatalog) types.Tool {
	return &LoadSkillTool{catalog: catalog}
}

func (t *LoadSkillTool) Definition() types.ToolDefinition {
	desc := "Load full SKILL.md instructions, skill directory path, and bundled helper files for a named skill."
	var names []string
	if t.catalog != nil {
		for _, s := range t.catalog.List() {
			if s.Name != "" {
				names = append(names, s.Name)
			}
		}
	}
	if len(names) > 0 {
		desc += " Available skills: " + strings.Join(names, ", ") + "."
	}

	prop := map[string]any{
		"type":        "string",
		"description": "Exact name of the skill to load.",
	}
	if len(names) > 0 {
		prop["enum"] = names
	}

	return types.ToolDefinition{
		Name:        "load_skill",
		Description: desc,
		Category:    types.ToolCategoryRead,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill_name": prop,
			},
			"required": []string{"skill_name"},
		},
	}
}

type loadSkillArgs struct {
	SkillName string `json:"skill_name"`
	Name      string `json:"name"`
	Skill     string `json:"skill"`
}

func (t *LoadSkillTool) Execute(_ context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	if t.catalog == nil {
		return types.ToolResult{
			Content:    "load_skill error: no skill catalog configured",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	var args loadSkillArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("invalid load_skill arguments: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	skillName := strings.TrimSpace(args.SkillName)
	if skillName == "" {
		skillName = strings.TrimSpace(args.Name)
	}
	if skillName == "" {
		skillName = strings.TrimSpace(args.Skill)
	}
	if skillName == "" {
		return types.ToolResult{
			Content:    "load_skill error: 'skill_name' is required",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	content, helperFiles, info, err := t.catalog.Load(skillName)
	if err != nil {
		var avail []string
		for _, s := range t.catalog.List() {
			avail = append(avail, s.Name)
		}
		suffix := ""
		if len(avail) > 0 {
			suffix = fmt.Sprintf(" (available skills: %s)", strings.Join(avail, ", "))
		}
		return types.ToolResult{
			Content:    fmt.Sprintf("load_skill error for %q: %v%s", skillName, err, suffix),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "<skill name=%q source=%q>\n", info.Name, info.Source)
	_, _ = fmt.Fprintf(&b, "Skill directory: %s\n", info.Dir)
	if info.SkillFile != "" {
		_, _ = fmt.Fprintf(&b, "Skill file: %s\n", info.SkillFile)
	}
	if len(helperFiles) > 0 {
		b.WriteString("Bundled helper files:\n")
		for _, hf := range helperFiles {
			_, _ = fmt.Fprintf(&b, "- %s\n", hf)
		}
		b.WriteByte('\n')
	} else {
		b.WriteString("Bundled helper files: (none)\n\n")
	}
	b.WriteString(strings.TrimSpace(content))
	b.WriteString("\n</skill>")

	return types.ToolResult{
		Content:    b.String(),
		DurationMs: time.Since(start).Milliseconds(),
		Metadata: map[string]any{
			"skill_name":   info.Name,
			"skill_dir":    info.Dir,
			"skill_file":   info.SkillFile,
			"helper_files": helperFiles,
		},
	}, nil
}

// --- CustomTool Adapter (.coder/tools/*.json) ---

var placeholderRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.-]+)\s*\}\}`)

// CustomTool wraps a types.CustomToolSpec as an executable types.Tool.
type CustomTool struct {
	spec           types.CustomToolSpec
	workspaceDir   string
	approver       types.Approver
	maxOutputBytes int
}

// NewCustomTool wraps a declarative CustomToolSpec, passing arguments via CODER_TOOL_ARGS and stdin,
// safely substituting {{argName}} placeholders as argv (unless spec.Shell is true), and enforcing approver rules.
func NewCustomTool(spec types.CustomToolSpec, workspaceDir string, approver types.Approver) types.Tool {
	return &CustomTool{
		spec:           spec,
		workspaceDir:   normalizeWorkspaceDir(workspaceDir),
		approver:       approver,
		maxOutputBytes: defaultMaxOutputBytes,
	}
}

func (t *CustomTool) Definition() types.ToolDefinition {
	params := t.spec.Parameters
	if len(params) == 0 {
		params = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	cat := types.ToolCategoryExecute
	if t.spec.ReadOnly {
		cat = types.ToolCategoryRead
	}
	return types.ToolDefinition{
		Name:        t.spec.Name,
		Description: t.spec.Description,
		Parameters:  params,
		Category:    cat,
	}
}

func (t *CustomTool) Execute(ctx context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	if strings.TrimSpace(t.spec.Command) == "" {
		return types.ToolResult{
			Content:    fmt.Sprintf("custom tool %q has an empty command", t.spec.Name),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	trimmedRaw := bytes.TrimSpace(rawArgs)
	if len(trimmedRaw) == 0 {
		trimmedRaw = []byte("{}")
	}

	argMap := make(map[string]any)
	dec := json.NewDecoder(bytes.NewReader(trimmedRaw))
	dec.UseNumber()
	if err := dec.Decode(&argMap); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("invalid JSON arguments for custom tool %q: %v", t.spec.Name, err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	workDir := t.workspaceDir
	if strings.TrimSpace(t.spec.WorkingDir) != "" {
		resolved, err := resolveToolPath(t.workspaceDir, t.spec.WorkingDir)
		if err != nil {
			return types.ToolResult{
				Content:    fmt.Sprintf("custom tool %q working_dir error: %v", t.spec.Name, err),
				IsError:    true,
				DurationMs: time.Since(start).Milliseconds(),
			}, nil
		}
		workDir = resolved
	}

	// Enforce ReadOnly and RequiresApproval semantics via approver.
	if t.approver != nil {
		needsCheck := true
		effectiveCategory := types.ToolCategoryExecute
		if t.spec.ReadOnly {
			effectiveCategory = types.ToolCategoryRead
		}

		if t.spec.RequiresApproval != nil {
			if *t.spec.RequiresApproval {
				// Explicit RequiresApproval=true forces execute-level confirmation (even if ReadOnly=true).
				effectiveCategory = types.ToolCategoryExecute
			} else {
				// Explicit RequiresApproval=false bypasses prompt in smart_auto/full_auto;
				// in strict_ask, non-readonly custom tools still confirm.
				if t.approver.Policy() != types.ApprovalStrictAsk || t.spec.ReadOnly {
					needsCheck = false
				}
			}
		}

		if needsCheck {
			approved, reason, err := t.approver.Check(ctx, types.ApprovalRequest{
				ToolName:   t.spec.Name,
				Category:   effectiveCategory,
				Summary:    fmt.Sprintf("Run custom tool %s (%s)", t.spec.Name, summarizeText(t.spec.Command, 80)),
				Command:    t.spec.Command,
				TargetPath: workDir,
				WorkingDir: workDir,
			})
			if err != nil {
				return types.ToolResult{
					Content:    fmt.Sprintf("custom tool %q permission error: %v", t.spec.Name, err),
					IsError:    true,
					DurationMs: time.Since(start).Milliseconds(),
				}, nil
			}
			if !approved {
				return types.ToolResult{
					Content:    fmt.Sprintf("custom tool %q permission denied (%s)", t.spec.Name, reason),
					IsError:    true,
					DurationMs: time.Since(start).Milliseconds(),
				}, nil
			}
		}
	}

	timeoutSec := t.spec.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultCmdTimeoutSec
	}
	if timeoutSec > maxCmdTimeoutSec {
		timeoutSec = maxCmdTimeoutSec
	}

	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if t.spec.Shell {
		shellCmd := substitutePlaceholders(t.spec.Command, argMap)
		if len(t.spec.Args) > 0 {
			rendered := make([]string, len(t.spec.Args))
			for i, a := range t.spec.Args {
				rendered[i] = substitutePlaceholders(a, argMap)
			}
			shellCmd = shellCmd + " " + strings.Join(rendered, " ")
		}
		cmd = buildShellCommand(cmdCtx, shellCmd)
	} else {
		exe := resolveCustomExecutable(t.spec.Command, workDir, t.spec.SourceFile)
		argv := make([]string, len(t.spec.Args))
		for i, a := range t.spec.Args {
			argv[i] = substitutePlaceholders(a, argMap)
		}
		cmd = exec.CommandContext(cmdCtx, exe, argv...)
		configureChildProcess(cmd)
	}

	cmd.Dir = workDir
	cmd.Stdin = bytes.NewReader(trimmedRaw)

	extraEnv := make(map[string]string, len(t.spec.Env)+2)
	for k, v := range t.spec.Env {
		extraEnv[k] = substitutePlaceholders(v, argMap)
	}
	extraEnv["CODER_TOOL_ARGS"] = string(trimmedRaw)
	extraEnv["CODER_WORKSPACE_DIR"] = t.workspaceDir

	// Include cached MSVC environment if already resolved on Windows, merged with custom env.
	resolveMSVCOnce()
	cmd.Env = buildMergedEnv(cachedMSVCMap, extraEnv)

	outBuf := newHeadTailBuffer(t.maxOutputBytes)
	cmd.Stdout = outBuf
	cmd.Stderr = outBuf

	execStart := time.Now()
	runErr := cmd.Run()
	execDurationMs := time.Since(execStart).Milliseconds()
	timedOut := errors.Is(cmdCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	exitCode := extractExitCode(runErr, timedOut)

	output := strings.TrimRight(outBuf.String(), "\r\n")
	if runErr != nil && output == "" {
		output = runErr.Error()
	}
	if timedOut {
		output = strings.TrimSpace(output + fmt.Sprintf("\n[custom tool %s timed out after %ds]", t.spec.Name, timeoutSec))
	}

	return types.ToolResult{
		Content:    output,
		IsError:    exitCode != 0 || timedOut || runErr != nil,
		DurationMs: time.Since(start).Milliseconds(),
		Metadata: map[string]any{
			"exit_code":   exitCode,
			"duration_ms": execDurationMs,
			"timed_out":   timedOut,
		},
	}, nil
}

func resolveCustomExecutable(command, workDir, sourceFile string) string {
	cmdTrim := strings.TrimSpace(command)
	if filepath.IsAbs(cmdTrim) {
		return filepath.Clean(cmdTrim)
	}
	if strings.ContainsAny(cmdTrim, `/\`) || strings.HasPrefix(cmdTrim, ".") {
		if sourceFile != "" {
			fromSource := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), cmdTrim))
			if fi, err := os.Stat(fromSource); err == nil && !fi.IsDir() {
				return fromSource
			}
		}
		return filepath.Clean(filepath.Join(workDir, cmdTrim))
	}
	return cmdTrim
}

func substitutePlaceholders(template string, argMap map[string]any) string {
	if !strings.Contains(template, "{{") {
		return template
	}
	return placeholderRe.ReplaceAllStringFunc(template, func(match string) string {
		sub := placeholderRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		val, ok := argMap[sub[1]]
		if !ok || val == nil {
			return ""
		}
		return formatArgValue(val)
	})
}

func formatArgValue(v any) string {
	switch tv := v.(type) {
	case string:
		return tv
	case json.Number:
		return tv.String()
	case bool:
		return strconv.FormatBool(tv)
	case float64:
		return strconv.FormatFloat(tv, 'f', -1, 64)
	default:
		b, err := json.Marshal(tv)
		if err != nil {
			return fmt.Sprint(tv)
		}
		return string(b)
	}
}
