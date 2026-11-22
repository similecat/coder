package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"coder/internal/types"
)

const (
	defaultListMaxDepth   = 3
	defaultListMaxEntries = 500
	defaultGrepMaxResults = 100
	maxGrepContextLines   = 5
	maxGrepFileSize       = 4 * 1024 * 1024 // 4MB skip threshold for pure-Go search
	maxGrepLineChars      = 500
)

// ListDirTool implements native bounded directory listing and glob discovery.
type ListDirTool struct {
	workspaceDir string
	approver     types.Approver
}

// NewListDirTool constructs the list_dir tool.
func NewListDirTool(workspaceDir string, approver types.Approver) types.Tool {
	return &ListDirTool{
		workspaceDir: normalizeWorkspaceDir(workspaceDir),
		approver:     approver,
	}
}

func (t *ListDirTool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "list_dir",
		Description: "List files and directories under a path with optional glob filtering (e.g. '**/*.cpp', '*.py') and max_depth control.",
		Category:    types.ToolCategoryRead,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path to list (defaults to workspace root '.').",
				},
				"pattern": map[string]any{
					"type":        "string",
					"description": "Optional glob filter (e.g. '**/*.cpp', '*.py', 'src/**/*.{h,hpp}').",
				},
				"max_depth": map[string]any{
					"type":        "integer",
					"description": "Optional maximum traversal depth (defaults to 3 when no pattern is set; unlimited up to 500 entries when pattern is set).",
				},
			},
		},
	}
}

type listDirArgs struct {
	Path     string `json:"path"`
	Pattern  string `json:"pattern"`
	MaxDepth *int   `json:"max_depth"`
}

func (t *ListDirTool) Execute(ctx context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	var args listDirArgs
	if len(bytes.TrimSpace(rawArgs)) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return types.ToolResult{
				Content:    fmt.Sprintf("invalid list_dir arguments: %v", err),
				IsError:    true,
				DurationMs: time.Since(start).Milliseconds(),
			}, nil
		}
	}

	userPath := strings.TrimSpace(args.Path)
	if userPath == "" {
		userPath = "."
	}
	absRoot, err := resolveToolPath(t.workspaceDir, userPath)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("list_dir path error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	dispRoot := displayPath(t.workspaceDir, absRoot)
	approved, reason, err := checkApproval(ctx, t.approver, types.ApprovalRequest{
		ToolName:   "list_dir",
		Category:   types.ToolCategoryRead,
		Summary:    fmt.Sprintf("List directory %s", dispRoot),
		TargetPath: absRoot,
		WorkingDir: t.workspaceDir,
	})
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("list_dir permission error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if !approved {
		return types.ToolResult{
			Content:    fmt.Sprintf("list_dir permission denied (%s): %s", reason, dispRoot),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	fi, err := os.Stat(absRoot)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("list_dir error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if !fi.IsDir() {
		return types.ToolResult{
			Content:    fmt.Sprintf("list_dir error: %s is a file (use read_file instead)", dispRoot),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	pattern := strings.TrimSpace(args.Pattern)
	maxDepth := 0 // 0 means unlimited depth
	if args.MaxDepth != nil && *args.MaxDepth > 0 {
		maxDepth = *args.MaxDepth
	} else if pattern == "" {
		maxDepth = defaultListMaxDepth
	}

	ignorer := newIgnoreMatcher(t.workspaceDir, userPath, pattern)
	var entries []string
	truncated := false

	walkErr := filepath.WalkDir(absRoot, func(curr string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subdirectories cleanly
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if curr == absRoot {
			return nil
		}

		relToRoot, err := filepath.Rel(absRoot, curr)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(relToRoot)
		depth := strings.Count(relSlash, "/") + 1

		if d.IsDir() {
			if ignorer.shouldSkipDir(curr, d.Name()) {
				return fs.SkipDir
			}
			if maxDepth > 0 && depth > maxDepth {
				return fs.SkipDir
			}
			if pattern == "" {
				if len(entries) >= defaultListMaxEntries {
					truncated = true
					return fs.SkipAll
				}
				entries = append(entries, relSlash+"/")
			}
			if maxDepth > 0 && depth == maxDepth {
				return fs.SkipDir
			}
			return nil
		}

		if maxDepth > 0 && depth > maxDepth {
			return nil
		}
		if pattern != "" && !MatchGlob(pattern, relSlash) {
			return nil
		}

		if len(entries) >= defaultListMaxEntries {
			truncated = true
			return fs.SkipAll
		}
		entries = append(entries, relSlash)
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll {
		return types.ToolResult{
			Content:    fmt.Sprintf("list_dir error: %v", walkErr),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	sort.Strings(entries)
	if len(entries) == 0 {
		msg := "(empty directory)"
		if pattern != "" {
			msg = fmt.Sprintf("No files matching pattern %q under %s", pattern, dispRoot)
		}
		return types.ToolResult{
			Content:    msg,
			DurationMs: time.Since(start).Milliseconds(),
			Metadata: map[string]any{
				"count":     0,
				"truncated": false,
			},
		}, nil
	}

	var b strings.Builder
	b.WriteString(strings.Join(entries, "\n"))
	if truncated {
		_, _ = fmt.Fprintf(&b, "\n... [Truncated at %d entries; narrow path, max_depth, or pattern]", defaultListMaxEntries)
	}

	return types.ToolResult{
		Content:    b.String(),
		DurationMs: time.Since(start).Milliseconds(),
		Metadata: map[string]any{
			"count":     len(entries),
			"truncated": truncated,
		},
	}, nil
}

// ListWorkspaceFiles returns up to limit slash-normalized relative file paths in workspaceDir,
// skipping build/VCS directories and root .gitignore directories.
func ListWorkspaceFiles(workspaceDir string, limit int) []string {
	ws := normalizeWorkspaceDir(workspaceDir)
	if limit <= 0 {
		limit = 2000
	}
	ignorer := newIgnoreMatcher(ws, "", "")
	files := make([]string, 0, min(limit, 256))

	_ = filepath.WalkDir(ws, func(curr string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if curr == ws {
			return nil
		}
		if d.IsDir() {
			if ignorer.shouldSkipDir(curr, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(ws, curr)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// --- Ripgrep Discovery & Pure-Go Concurrent Regex Search ---

var (
	rgOnce sync.Once
	rgPath string
)

// ResolveRipgrepPath locates rg / rg.exe on PATH or in VS Code's bundled installation once per process.
func ResolveRipgrepPath() string {
	rgOnce.Do(func() {
		rgPath = discoverRipgrep()
	})
	return rgPath
}

func discoverRipgrep() string {
	if p, err := exec.LookPath("rg"); err == nil && p != "" {
		return p
	}
	if runtime.GOOS != "windows" {
		return ""
	}

	var roots []string
	if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
		roots = append(roots,
			filepath.Join(localApp, "Programs", "Microsoft VS Code"),
			filepath.Join(localApp, "Programs", "Microsoft VS Code Insiders"),
		)
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		roots = append(roots,
			filepath.Join(pf, "Microsoft VS Code"),
			filepath.Join(pf, "Microsoft VS Code Insiders"),
		)
	}

	subPatterns := []string{
		`resources\app\node_modules.asar.unpacked\@vscode\ripgrep\bin\rg.exe`,
		`resources\app\node_modules.asar.unpacked\@vscode\ripgrep-universal\bin\win32-x64\rg.exe`,
		`resources\app\node_modules\@vscode\ripgrep\bin\rg.exe`,
		`*\resources\app\node_modules.asar.unpacked\@vscode\ripgrep\bin\rg.exe`,
		`*\resources\app\node_modules.asar.unpacked\@vscode\ripgrep-universal\bin\win32-x64\rg.exe`,
		`*\resources\app\node_modules\@vscode\ripgrep\bin\rg.exe`,
	}

	for _, root := range roots {
		for _, sub := range subPatterns {
			matches, err := filepath.Glob(filepath.Join(root, sub))
			if err != nil {
				continue
			}
			for _, m := range matches {
				if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
					return m
				}
			}
		}
	}
	return ""
}

// GrepOptions configures a regex search across a workspace directory or file.
type GrepOptions struct {
	WorkspaceDir    string
	TargetPath      string
	ExplicitSubPath string
	Pattern         string
	Glob            string
	CaseInsensitive bool
	ContextLines    int
	MaxResults      int
}

// GrepSearchTool implements fast regex search with an rg fast-path and a pure-Go concurrent worker-pool engine.
type GrepSearchTool struct {
	workspaceDir string
	approver     types.Approver
	disableRg    bool
}

// NewGrepSearchTool constructs the grep_search tool.
func NewGrepSearchTool(workspaceDir string, approver types.Approver) types.Tool {
	return &GrepSearchTool{
		workspaceDir: normalizeWorkspaceDir(workspaceDir),
		approver:     approver,
	}
}

func newGrepSearchToolWithOptions(workspaceDir string, approver types.Approver, disableRg bool) types.Tool {
	return &GrepSearchTool{
		workspaceDir: normalizeWorkspaceDir(workspaceDir),
		approver:     approver,
		disableRg:    disableRg,
	}
}

func (t *GrepSearchTool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        "grep_search",
		Description: "Search file contents with regular expressions, returning 'relpath:line: content'. Skips binary files and build/VCS folders.",
		Category:    types.ToolCategoryRead,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Regular expression pattern to search for.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Optional file or subdirectory path to search within (defaults to workspace root).",
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "Optional file glob filter (e.g. '*.cpp', '*.py', 'src/**/*.go').",
				},
				"case_insensitive": map[string]any{
					"type":        "boolean",
					"description": "Perform case-insensitive matching (default: false).",
				},
				"context_lines": map[string]any{
					"type":        "integer",
					"description": "Number of surrounding context lines before/after each match (0-5, default: 0).",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Maximum number of matching lines to return (default: 100).",
				},
			},
			"required": []string{"pattern"},
		},
	}
}

type grepSearchArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	CaseInsensitive bool   `json:"case_insensitive"`
	ContextLines    int    `json:"context_lines"`
	MaxResults      int    `json:"max_results"`
}

func (t *GrepSearchTool) Execute(ctx context.Context, rawArgs json.RawMessage) (types.ToolResult, error) {
	start := time.Now()
	var args grepSearchArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("invalid grep_search arguments: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if args.Pattern == "" {
		return types.ToolResult{
			Content:    "grep_search error: 'pattern' must not be empty",
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	userPath := strings.TrimSpace(args.Path)
	if userPath == "" {
		userPath = "."
	}
	absTarget, err := resolveToolPath(t.workspaceDir, userPath)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("grep_search path error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	dispTarget := displayPath(t.workspaceDir, absTarget)
	approved, reason, err := checkApproval(ctx, t.approver, types.ApprovalRequest{
		ToolName:   "grep_search",
		Category:   types.ToolCategoryRead,
		Summary:    fmt.Sprintf("Search %q in %s", args.Pattern, dispTarget),
		TargetPath: absTarget,
		WorkingDir: t.workspaceDir,
	})
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("grep_search permission error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	if !approved {
		return types.ToolResult{
			Content:    fmt.Sprintf("grep_search permission denied (%s): %s", reason, dispTarget),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	fi, err := os.Stat(absTarget)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("grep_search error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	ctxLines := min(max(args.ContextLines, 0), maxGrepContextLines)
	maxRes := args.MaxResults
	if maxRes <= 0 {
		maxRes = defaultGrepMaxResults
	}
	if maxRes > 1000 {
		maxRes = 1000
	}

	opts := GrepOptions{
		WorkspaceDir:    t.workspaceDir,
		TargetPath:      absTarget,
		ExplicitSubPath: userPath,
		Pattern:         args.Pattern,
		Glob:            strings.TrimSpace(args.Glob),
		CaseInsensitive: args.CaseInsensitive,
		ContextLines:    ctxLines,
		MaxResults:      maxRes,
	}

	// Fast-path: use ripgrep for directory searches when available and enabled;
	// for single files or if rg fails/is unavailable, run pure-Go concurrent engine directly.
	engine := "pure_go"
	if !t.disableRg && fi.IsDir() {
		if rgBin := ResolveRipgrepPath(); rgBin != "" {
			if out, matchCount, truncated, rgErr := grepWithRipgrep(ctx, rgBin, opts); rgErr == nil {
				engine = "ripgrep"
				return formatGrepToolResult(out, matchCount, truncated, engine, start), nil
			}
		}
	}

	out, matchCount, truncated, err := GrepPureGo(ctx, opts)
	if err != nil {
		return types.ToolResult{
			Content:    fmt.Sprintf("grep_search error: %v", err),
			IsError:    true,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	return formatGrepToolResult(out, matchCount, truncated, engine, start), nil
}

func formatGrepToolResult(out string, matchCount int, truncated bool, engine string, start time.Time) types.ToolResult {
	if matchCount == 0 || strings.TrimSpace(out) == "" {
		return types.ToolResult{
			Content:    "No matches found.",
			DurationMs: time.Since(start).Milliseconds(),
			Metadata: map[string]any{
				"matches":   0,
				"truncated": false,
				"engine":    engine,
			},
		}
	}
	return types.ToolResult{
		Content:    out,
		DurationMs: time.Since(start).Milliseconds(),
		Metadata: map[string]any{
			"matches":   matchCount,
			"truncated": truncated,
			"engine":    engine,
		},
	}
}

func grepWithRipgrep(ctx context.Context, rgBin string, opts GrepOptions) (string, int, bool, error) {
	rgCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var cmdArgs []string
	cmdArgs = append(cmdArgs,
		"--no-config",
		"--line-number",
		"--with-filename",
		"--color=never",
		"--no-heading",
		"--path-separator=/",
	)
	if opts.CaseInsensitive {
		cmdArgs = append(cmdArgs, "-i")
	}
	if opts.ContextLines > 0 {
		cmdArgs = append(cmdArgs, "-C", strconv.Itoa(opts.ContextLines))
	}

	// Apply default skipped directories unless explicitly targeted.
	ignorer := newIgnoreMatcher(opts.WorkspaceDir, opts.ExplicitSubPath, opts.Glob)
	for dirName := range defaultSkippedDirs {
		if !ignorer.shouldSkipDir("", dirName) {
			continue
		}
		cmdArgs = append(cmdArgs, "-g", "!"+dirName+"/**")
	}
	if opts.Glob != "" {
		cmdArgs = append(cmdArgs, "-g", opts.Glob)
	}

	cmdArgs = append(cmdArgs, "-e", opts.Pattern, opts.TargetPath)
	cmd := exec.CommandContext(rgCtx, rgBin, cmdArgs...)
	configureChildProcess(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && stderr.Len() == 0 {
			// Exit code 1 with empty stderr in rg means 0 matches found.
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("rg failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	ws := normalizeWorkspaceDir(opts.WorkspaceDir)
	rawLines := strings.Split(strings.TrimRight(stdout.String(), "\r\n"), "\n")
	var outLines []string
	matchCount := 0
	truncated := false

	for _, raw := range rawLines {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		if line == "--" {
			if opts.ContextLines > 0 && len(outLines) > 0 && outLines[len(outLines)-1] != "--" {
				outLines = append(outLines, "--")
			}
			continue
		}

		normalized, isMatchLine := normalizeRgLine(ws, line)
		if isMatchLine {
			if matchCount >= opts.MaxResults {
				truncated = true
				break
			}
			matchCount++
		} else if matchCount >= opts.MaxResults && opts.ContextLines == 0 {
			truncated = true
			break
		}
		outLines = append(outLines, normalized)
	}

	// Trim trailing context separator if present.
	for len(outLines) > 0 && outLines[len(outLines)-1] == "--" {
		outLines = outLines[:len(outLines)-1]
	}

	var b strings.Builder
	b.WriteString(strings.Join(outLines, "\n"))
	if truncated {
		_, _ = fmt.Fprintf(&b, "\n... [Truncated at %d matches; refine pattern, path, or glob]", opts.MaxResults)
	}
	return b.String(), matchCount, truncated, nil
}

func normalizeRgLine(ws, line string) (string, bool) {
	// On Windows an absolute path starts with "C:/..." or "C:\...", so skip drive colon at index 1.
	searchStart := 0
	if len(line) >= 3 && line[1] == ':' && (line[2] == '/' || line[2] == '\\') {
		searchStart = 2
	}
	rem := line[searchStart:]

	// Find first ':' or '-' followed by digits and another ':' or '-'.
	for i := 0; i < len(rem); i++ {
		sep1 := rem[i]
		if sep1 != ':' && sep1 != '-' {
			continue
		}
		j := i + 1
		for j < len(rem) && rem[j] >= '0' && rem[j] <= '9' {
			j++
		}
		if j == i+1 || j >= len(rem) {
			continue
		}
		sep2 := rem[j]
		if sep2 != sep1 {
			continue
		}
		filePart := line[:searchStart+i]
		lineNumStr := rem[i+1 : j]
		contentPart := rem[j+1:]
		if len(contentPart) > maxGrepLineChars {
			contentPart = contentPart[:maxGrepLineChars] + "..."
		}

		absCandidate := filepath.FromSlash(filePart)
		relDisp := filepath.ToSlash(displayPath(ws, absCandidate))
		if sep1 == ':' {
			return fmt.Sprintf("%s:%s: %s", relDisp, lineNumStr, contentPart), true
		}
		return fmt.Sprintf("%s:%s- %s", relDisp, lineNumStr, contentPart), false
	}
	return line, true
}

type fileGrepResult struct {
	relPath string
	lines   []string
	matches int
}

// GrepPureGo runs a pure-Go concurrent worker-pool regex search across opts.TargetPath.
// It skips binary files and ignored directories, returning formatted 'relpath:line: content' results.
func GrepPureGo(ctx context.Context, opts GrepOptions) (output string, matchCount int, truncated bool, err error) {
	ws := normalizeWorkspaceDir(opts.WorkspaceDir)
	target := opts.TargetPath
	if target == "" {
		target = ws
	}
	maxRes := opts.MaxResults
	if maxRes <= 0 {
		maxRes = defaultGrepMaxResults
	}
	ctxLines := min(max(opts.ContextLines, 0), maxGrepContextLines)

	expr := opts.Pattern
	if opts.CaseInsensitive && !strings.HasPrefix(expr, "(?i)") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", 0, false, fmt.Errorf("invalid regex %q: %w", opts.Pattern, err)
	}

	fi, err := os.Stat(target)
	if err != nil {
		return "", 0, false, err
	}

	// Single-file fast path (zero goroutine/channel overhead).
	if !fi.IsDir() {
		rel := filepath.ToSlash(displayPath(ws, target))
		if opts.Glob != "" && !MatchGlob(opts.Glob, rel) {
			return "", 0, false, nil
		}
		res, ok := searchSingleFile(target, rel, re, ctxLines, maxRes+1)
		if !ok || res.matches == 0 {
			return "", 0, false, nil
		}
		return assembleFileGrepResults([]fileGrepResult{res}, maxRes, ctxLines)
	}

	// Collect candidate files in deterministic walk order.
	ignorer := newIgnoreMatcher(ws, opts.ExplicitSubPath, opts.Glob)
	type candidateFile struct {
		abs string
		rel string
	}
	var candidates []candidateFile

	walkErr := filepath.WalkDir(target, func(curr string, d fs.DirEntry, wErr error) error {
		if wErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if curr == target {
			return nil
		}
		if d.IsDir() {
			if ignorer.shouldSkipDir(curr, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		// Relative to target for glob matching, relative to workspace for display.
		relForGlob, err := filepath.Rel(target, curr)
		if err != nil {
			return nil
		}
		if opts.Glob != "" && !MatchGlob(opts.Glob, filepath.ToSlash(relForGlob)) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > maxGrepFileSize {
			return nil
		}

		relDisp := filepath.ToSlash(displayPath(ws, curr))
		candidates = append(candidates, candidateFile{abs: curr, rel: relDisp})
		return nil
	})
	if walkErr != nil {
		return "", 0, false, walkErr
	}
	if len(candidates) == 0 {
		return "", 0, false, nil
	}

	numWorkers := min(max(runtime.NumCPU(), 2), 8)
	if len(candidates) < numWorkers {
		numWorkers = len(candidates)
	}

	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan candidateFile, min(len(candidates), 256))
	resultsCh := make(chan fileGrepResult, min(len(candidates), 256))

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cand := range jobs {
				if searchCtx.Err() != nil {
					return
				}
				if res, ok := searchSingleFile(cand.abs, cand.rel, re, ctxLines, maxRes+1); ok && res.matches > 0 {
					resultsCh <- res
				}
			}
		}()
	}

	go func() {
		for _, c := range candidates {
			if searchCtx.Err() != nil {
				break
			}
			jobs <- c
		}
		close(jobs)
		wg.Wait()
		close(resultsCh)
	}()

	var collected []fileGrepResult
	for r := range resultsCh {
		collected = append(collected, r)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, false, err
	}
	if len(collected) == 0 {
		return "", 0, false, nil
	}

	sort.Slice(collected, func(i, j int) bool {
		return collected[i].relPath < collected[j].relPath
	})
	return assembleFileGrepResults(collected, maxRes, ctxLines)
}

func searchSingleFile(absPath, relPath string, re *regexp.Regexp, ctxLines, perFileLimit int) (fileGrepResult, bool) {
	data, err := os.ReadFile(absPath)
	if err != nil || len(data) == 0 || len(data) > maxGrepFileSize {
		return fileGrepResult{}, false
	}
	if isBinarySample(data) {
		return fileGrepResult{}, false
	}

	// Quick pre-check on whole buffer before splitting lines!
	if !re.Match(data) {
		return fileGrepResult{}, false
	}

	rawLines := strings.Split(string(data), "\n")
	// Drop empty trailing element produced by trailing '\n'.
	if len(rawLines) > 0 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}

	if ctxLines == 0 {
		var formatted []string
		matches := 0
		for i, raw := range rawLines {
			line := strings.TrimSuffix(raw, "\r")
			if re.MatchString(line) {
				matches++
				disp := line
				if len(disp) > maxGrepLineChars {
					disp = disp[:maxGrepLineChars] + "..."
				}
				formatted = append(formatted, fmt.Sprintf("%s:%d: %s", relPath, i+1, disp))
				if perFileLimit > 0 && matches >= perFileLimit {
					break
				}
			}
		}
		return fileGrepResult{relPath: relPath, lines: formatted, matches: matches}, matches > 0
	}

	// Context-aware formatting.
	matchedIdx := make([]int, 0, 8)
	matchSet := make(map[int]bool)
	for i, raw := range rawLines {
		line := strings.TrimSuffix(raw, "\r")
		if re.MatchString(line) {
			matchedIdx = append(matchedIdx, i)
			matchSet[i] = true
			if perFileLimit > 0 && len(matchedIdx) >= perFileLimit {
				break
			}
		}
	}
	if len(matchedIdx) == 0 {
		return fileGrepResult{}, false
	}

	include := make([]bool, len(rawLines))
	for _, idx := range matchedIdx {
		lo := max(0, idx-ctxLines)
		hi := min(len(rawLines)-1, idx+ctxLines)
		for k := lo; k <= hi; k++ {
			include[k] = true
		}
	}

	var formatted []string
	prevPrinted := -2
	for i := 0; i < len(rawLines); i++ {
		if !include[i] {
			continue
		}
		if prevPrinted >= 0 && i > prevPrinted+1 {
			formatted = append(formatted, "--")
		}
		line := strings.TrimSuffix(rawLines[i], "\r")
		if len(line) > maxGrepLineChars {
			line = line[:maxGrepLineChars] + "..."
		}
		if matchSet[i] {
			formatted = append(formatted, fmt.Sprintf("%s:%d: %s", relPath, i+1, line))
		} else {
			formatted = append(formatted, fmt.Sprintf("%s:%d- %s", relPath, i+1, line))
		}
		prevPrinted = i
	}

	return fileGrepResult{
		relPath: relPath,
		lines:   formatted,
		matches: len(matchedIdx),
	}, true
}

func assembleFileGrepResults(results []fileGrepResult, maxRes, ctxLines int) (string, int, bool, error) {
	var out []string
	totalMatches := 0
	truncated := false

	for fileIdx, r := range results {
		if ctxLines > 0 && fileIdx > 0 && len(out) > 0 && out[len(out)-1] != "--" {
			out = append(out, "--")
		}
		for _, line := range r.lines {
			isSep := line == "--"
			isMatch := !isSep && isFormattedMatchLine(r.relPath, line)
			if isMatch {
				if totalMatches >= maxRes {
					truncated = true
					break
				}
				totalMatches++
			}
			out = append(out, line)
		}
		if truncated {
			break
		}
	}

	for len(out) > 0 && out[len(out)-1] == "--" {
		out = out[:len(out)-1]
	}

	var b strings.Builder
	b.WriteString(strings.Join(out, "\n"))
	if truncated {
		_, _ = fmt.Fprintf(&b, "\n... [Truncated at %d matches; refine pattern, path, or glob]", maxRes)
	}
	return b.String(), totalMatches, truncated, nil
}

func isFormattedMatchLine(relPath, formatted string) bool {
	if !strings.HasPrefix(formatted, relPath+":") {
		return false
	}
	rest := formatted[len(relPath)+1:]
	idx := 0
	for idx < len(rest) && rest[idx] >= '0' && rest[idx] <= '9' {
		idx++
	}
	return idx > 0 && idx < len(rest) && rest[idx] == ':'
}
