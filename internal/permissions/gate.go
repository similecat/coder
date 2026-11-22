package permissions

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"coder/internal/types"
)

// PromptFunc prompts a human operator (or test harness) to approve or deny a tool action.
type PromptFunc func(ctx context.Context, req types.ApprovalRequest) (bool, error)

// GateOptions configures a permission Gate.
type GateOptions struct {
	WorkspaceDir string
	Policy       types.ApprovalPolicy
	PromptFunc   PromptFunc
	// Interactive overrides automatic terminal detection on os.Stdin when non-nil.
	Interactive *bool
	// Stdin and Stderr override os.Stdin and os.Stderr for the default terminal reader.
	Stdin  io.Reader
	Stderr io.Writer
}

// Gate implements types.Approver with workspace boundary checks and configurable approval policies.
type Gate struct {
	mu               sync.RWMutex
	promptMu         sync.Mutex
	workspaceDir     string
	policy           types.ApprovalPolicy
	PromptFunc       PromptFunc
	interactive      *bool
	stdin            io.Reader
	stderr           io.Writer
	stdinReader      *bufio.Reader
	approvedPrefixes map[string]struct{}
}

var _ types.Approver = (*Gate)(nil)

// NewGate creates a thread-safe permission Gate for workspaceDir under policy.
// An optional custom PromptFunc may be supplied; if omitted, Gate falls back to
// an interactive terminal prompt on os.Stdin/os.Stderr (or safe denial when non-interactive).
func NewGate(workspaceDir string, policy types.ApprovalPolicy, promptFn ...PromptFunc) *Gate {
	opts := GateOptions{
		WorkspaceDir: workspaceDir,
		Policy:       policy,
	}
	if len(promptFn) > 0 {
		opts.PromptFunc = promptFn[0]
	}
	return NewGateWithOptions(opts)
}

// NewGateWithOptions creates a Gate with full configuration options.
func NewGateWithOptions(opts GateOptions) *Gate {
	pol := normalizePolicy(opts.Policy)
	ws := strings.TrimSpace(opts.WorkspaceDir)
	if ws != "" {
		if abs, err := filepath.Abs(filepath.Clean(ws)); err == nil {
			ws = abs
		}
	}
	return &Gate{
		workspaceDir:     ws,
		policy:           pol,
		PromptFunc:       opts.PromptFunc,
		interactive:      opts.Interactive,
		stdin:            opts.Stdin,
		stderr:           opts.Stderr,
		approvedPrefixes: make(map[string]struct{}),
	}
}

func normalizePolicy(policy types.ApprovalPolicy) types.ApprovalPolicy {
	switch policy {
	case types.ApprovalFullAuto, types.ApprovalSmartAuto, types.ApprovalStrictAsk:
		return policy
	default:
		return types.ApprovalSmartAuto
	}
}

// Policy returns the active ApprovalPolicy.
func (g *Gate) Policy() types.ApprovalPolicy {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return normalizePolicy(g.policy)
}

// SetPolicy updates the active ApprovalPolicy safely across goroutines.
func (g *Gate) SetPolicy(policy types.ApprovalPolicy) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.policy = normalizePolicy(policy)
}

// WorkspaceDir returns the configured workspace root directory.
func (g *Gate) WorkspaceDir() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.workspaceDir
}

// SetWorkspaceDir updates the workspace root directory.
func (g *Gate) SetWorkspaceDir(workspaceDir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ws := strings.TrimSpace(workspaceDir)
	if ws != "" {
		if abs, err := filepath.Abs(filepath.Clean(ws)); err == nil {
			ws = abs
		}
	}
	g.workspaceDir = ws
}

// SetPromptFunc configures a custom approval prompt callback.
func (g *Gate) SetPromptFunc(fn PromptFunc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.PromptFunc = fn
}

// AllowCommandPrefix records a command prefix (such as "go" or "pytest") as approved for this session.
func (g *Gate) AllowCommandPrefix(prefix string) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return
	}
	if runtime.GOOS == "windows" {
		prefix = strings.ToLower(prefix)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.approvedPrefixes == nil {
		g.approvedPrefixes = make(map[string]struct{})
	}
	g.approvedPrefixes[prefix] = struct{}{}
}

func (g *Gate) isCommandPrefixApproved(command string) bool {
	prefix := extractCommandPrefix(command)
	if prefix == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		prefix = strings.ToLower(prefix)
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.approvedPrefixes[prefix]
	return ok
}

func extractCommandPrefix(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	tok := strings.Trim(fields[0], `"'`)
	return filepath.Base(tok)
}

// Check evaluates whether req is permitted under the active policy.
func (g *Gate) Check(ctx context.Context, req types.ApprovalRequest) (bool, string, error) {
	if err := ctx.Err(); err != nil {
		return false, "context_canceled", err
	}

	g.mu.RLock()
	policy := normalizePolicy(g.policy)
	wsDir := g.workspaceDir
	promptFn := g.PromptFunc
	g.mu.RUnlock()

	if wsDir == "" && req.WorkingDir != "" {
		wsDir = req.WorkingDir
	}

	// 1. Full-auto always approves everything immediately.
	if policy == types.ApprovalFullAuto {
		return true, "full_auto", nil
	}

	// 2. Read category rules:
	//    - Empty target or target within workspace auto-approves under ALL policies.
	//    - Out-of-workspace reads prompt in strict_ask, and auto-approve in smart_auto
	//      unless targeting sensitive dotfiles/credential directories.
	if req.Category == types.ToolCategoryRead {
		if req.TargetPath == "" || IsWithinWorkspace(wsDir, req.TargetPath) {
			return true, "workspace_read", nil
		}
		if policy == types.ApprovalSmartAuto && !IsSensitivePath(resolveCandidatePath(wsDir, req.TargetPath)) {
			return true, "smart_auto_external_read", nil
		}
		return g.prompt(ctx, promptFn, req)
	}

	// 3. Smart-auto rules for Edit and Execute:
	if policy == types.ApprovalSmartAuto {
		switch req.Category {
		case types.ToolCategoryEdit:
			if req.TargetPath != "" && IsWithinWorkspace(wsDir, req.TargetPath) {
				return true, "smart_auto_workspace_edit", nil
			}
			return g.prompt(ctx, promptFn, req)
		case types.ToolCategoryExecute:
			if req.Command != "" && g.isCommandPrefixApproved(req.Command) {
				return true, "approved_command_prefix", nil
			}
			return g.prompt(ctx, promptFn, req)
		default:
			return g.prompt(ctx, promptFn, req)
		}
	}

	// 4. Strict-ask prompts for all Edit, Execute, and any other non-read actions.
	return g.prompt(ctx, promptFn, req)
}

func (g *Gate) prompt(ctx context.Context, promptFn PromptFunc, req types.ApprovalRequest) (bool, string, error) {
	if promptFn != nil {
		ok, err := promptFn(ctx, req)
		if err != nil {
			return false, "prompt_error", err
		}
		if ok {
			return true, "user_approved", nil
		}
		return false, "user_denied", nil
	}

	return g.defaultTerminalPrompt(ctx, req)
}

func (g *Gate) isInteractiveTerminal() bool {
	g.mu.RLock()
	override := g.interactive
	customStdin := g.stdin
	g.mu.RUnlock()

	if override != nil {
		return *override
	}
	if customStdin != nil {
		if f, ok := customStdin.(*os.File); ok {
			return isCharDevice(f)
		}
		// Custom non-file io.Reader supplied explicitly (e.g., unit tests).
		return true
	}
	return isCharDevice(os.Stdin)
}

func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func (g *Gate) defaultTerminalPrompt(ctx context.Context, req types.ApprovalRequest) (bool, string, error) {
	if !g.isInteractiveTerminal() {
		return false, "non_interactive_denied", nil
	}

	g.promptMu.Lock()
	defer g.promptMu.Unlock()

	// Re-check policy in case an earlier queued prompt upgraded the session with "always".
	if g.Policy() == types.ApprovalFullAuto {
		return true, "full_auto", nil
	}
	if req.Category == types.ToolCategoryExecute && req.Command != "" && g.isCommandPrefixApproved(req.Command) {
		return true, "approved_command_prefix", nil
	}

	g.mu.Lock()
	in := g.stdin
	if in == nil {
		in = os.Stdin
	}
	out := g.stderr
	if out == nil {
		out = os.Stderr
	}
	if g.stdinReader == nil {
		g.stdinReader = bufio.NewReader(in)
	}
	reader := g.stdinReader
	g.mu.Unlock()

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
	_, _ = fmt.Fprintf(out, "[permission] %s (%s): %s — Allow? [y/N/a(lways)]: ", req.ToolName, req.Category, detail)

	type readRes struct {
		line string
		err  error
	}
	ch := make(chan readRes, 1)
	go func() {
		line, err := reader.ReadString('\n')
		ch <- readRes{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return false, "context_canceled", ctx.Err()
	case res := <-ch:
		if res.err != nil && res.err != io.EOF {
			return false, "prompt_read_error", res.err
		}
		ans := strings.ToLower(strings.TrimSpace(res.line))
		switch ans {
		case "y", "yes":
			return true, "user_approved", nil
		case "a", "always":
			if req.Category == types.ToolCategoryExecute && req.Command != "" {
				g.AllowCommandPrefix(extractCommandPrefix(req.Command))
			}
			g.SetPolicy(types.ApprovalFullAuto)
			return true, "user_approved_always", nil
		default:
			return false, "user_denied", nil
		}
	}
}

// IsWithinWorkspace reports whether targetPath resolves inside workspaceDir (or is workspaceDir itself).
// Relative paths are resolved relative to workspaceDir. Existing ancestor symlinks/junctions are
// evaluated via filepath.EvalSymlinks to block symlink escapes and ..\ traversal.
func IsWithinWorkspace(workspaceDir, targetPath string) bool {
	if strings.TrimSpace(workspaceDir) == "" || targetPath == "" {
		return false
	}
	if strings.ContainsRune(workspaceDir, 0) || strings.ContainsRune(targetPath, 0) {
		return false
	}

	absWs, err := filepath.Abs(filepath.Clean(workspaceDir))
	if err != nil {
		return false
	}

	absTarget, ok := resolveTargetAbs(absWs, targetPath)
	if !ok {
		return false
	}

	realWs := normalizeExtendedPath(resolveExistingPrefixSymlinks(absWs))
	realTarget := normalizeExtendedPath(resolveExistingPrefixSymlinks(absTarget))

	wsComp := realWs
	targetComp := realTarget
	if runtime.GOOS == "windows" {
		wsComp = strings.ToLower(wsComp)
		targetComp = strings.ToLower(targetComp)
		if filepath.VolumeName(wsComp) == "" || filepath.VolumeName(wsComp) != filepath.VolumeName(targetComp) {
			return false
		}
	}

	rel, err := filepath.Rel(wsComp, targetComp)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

func resolveCandidatePath(workspaceDir, targetPath string) string {
	if workspaceDir != "" {
		if absWs, err := filepath.Abs(filepath.Clean(workspaceDir)); err == nil {
			if absTarget, ok := resolveTargetAbs(absWs, targetPath); ok {
				return resolveExistingPrefixSymlinks(absTarget)
			}
		}
	}
	if abs, err := filepath.Abs(filepath.Clean(targetPath)); err == nil {
		return resolveExistingPrefixSymlinks(abs)
	}
	return filepath.Clean(targetPath)
}

func resolveTargetAbs(absWs, targetPath string) (string, bool) {
	if filepath.IsAbs(targetPath) {
		abs, err := filepath.Abs(filepath.Clean(targetPath))
		return abs, err == nil
	}

	if runtime.GOOS == "windows" {
		// Reject or resolve drive-relative paths like "C:foo" against process state rather than joining blindly.
		if vol := filepath.VolumeName(targetPath); vol != "" {
			abs, err := filepath.Abs(filepath.Clean(targetPath))
			return abs, err == nil
		}
		// Rooted path without drive letter (e.g. `\Windows\System32` or `/Windows/System32`) refers to root of drive.
		if strings.HasPrefix(targetPath, `\`) || strings.HasPrefix(targetPath, `/`) {
			vol := filepath.VolumeName(absWs)
			abs, err := filepath.Abs(vol + filepath.Clean(targetPath))
			return abs, err == nil
		}
	}

	joined := filepath.Clean(filepath.Join(absWs, targetPath))
	abs, err := filepath.Abs(joined)
	return abs, err == nil
}

// resolveExistingPrefixSymlinks walks up from absPath until an existing ancestor is found,
// resolves symlinks/junctions/short-8.3 names on that ancestor, and re-attaches non-existent tail components.
func resolveExistingPrefixSymlinks(absPath string) string {
	cur := filepath.Clean(absPath)
	var tail []string
	for {
		resolved, err := evalRealPath(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(absPath)
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func normalizeExtendedPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return `\\` + p[len(`\\?\UNC\`):]
	}
	if strings.HasPrefix(p, `\\?\`) && len(p) >= 6 && p[5] == ':' {
		return p[4:]
	}
	return p
}

// IsSensitivePath reports whether targetPath points to sensitive dotfile credentials or keys
// (such as ~/.ssh, ~/.aws, ~/.gnupg, ~/.kube, .env, .netrc, .npmrc, .git-credentials).
func IsSensitivePath(targetPath string) bool {
	if targetPath == "" {
		return false
	}
	norm := strings.ReplaceAll(filepath.Clean(targetPath), `\`, `/`)
	if runtime.GOOS == "windows" {
		norm = strings.ToLower(norm)
	} else {
		norm = strings.ToLower(norm)
	}

	parts := strings.Split(norm, "/")
	for i, seg := range parts {
		switch seg {
		case ".ssh", ".aws", ".gnupg", ".kube", ".azure", ".gcloud", ".password-store":
			return true
		case ".docker":
			if i+1 < len(parts) && parts[i+1] == "config.json" {
				return true
			}
		case "gh":
			if i > 0 && parts[i-1] == ".config" && i+1 < len(parts) && parts[i+1] == "hosts.yml" {
				return true
			}
		}
	}

	base := parts[len(parts)-1]
	switch base {
	case ".env", ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".pgpass", ".my.cnf",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa":
		return true
	}
	if strings.HasPrefix(base, ".env.") {
		switch base {
		case ".env.example", ".env.sample", ".env.template":
			return false
		default:
			return true
		}
	}
	return false
}
