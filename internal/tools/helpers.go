package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"coder/internal/types"
)

const (
	defaultMaxOutputBytes = 64 * 1024 // 64KB head+tail command output cap
)

var defaultSkippedDirs = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"__pycache__":  {},
	"bin":          {},
	"obj":          {},
	"dist":         {},
	".vs":          {},
}

func normalizeWorkspaceDir(workspaceDir string) string {
	ws := strings.TrimSpace(workspaceDir)
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil {
			return filepath.Clean(cwd)
		}
		return "."
	}
	if abs, err := filepath.Abs(filepath.Clean(ws)); err == nil {
		return abs
	}
	return filepath.Clean(ws)
}

func resolveToolPath(workspaceDir, userPath string) (string, error) {
	ws := normalizeWorkspaceDir(workspaceDir)
	p := strings.TrimSpace(userPath)
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path containing NUL byte")
	}

	if filepath.IsAbs(p) {
		return filepath.Abs(filepath.Clean(p))
	}
	if runtime.GOOS == "windows" {
		if vol := filepath.VolumeName(p); vol != "" {
			return filepath.Abs(filepath.Clean(p))
		}
		if strings.HasPrefix(p, `\`) || strings.HasPrefix(p, `/`) {
			vol := filepath.VolumeName(ws)
			return filepath.Abs(vol + filepath.Clean(p))
		}
	}
	return filepath.Abs(filepath.Clean(filepath.Join(ws, p)))
}

func displayPath(workspaceDir, absPath string) string {
	ws := normalizeWorkspaceDir(workspaceDir)
	wsComp := ws
	targetComp := absPath
	if runtime.GOOS == "windows" {
		wsComp = strings.ToLower(wsComp)
		targetComp = strings.ToLower(targetComp)
		if filepath.VolumeName(wsComp) != filepath.VolumeName(targetComp) {
			return absPath
		}
	}
	rel, err := filepath.Rel(wsComp, targetComp)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return absPath
	}
	// Compute relative preserving actual casing on disk/input.
	if origRel, err := filepath.Rel(ws, absPath); err == nil {
		return origRel
	}
	return rel
}

func checkApproval(ctx context.Context, approver types.Approver, req types.ApprovalRequest) (bool, string, error) {
	if approver == nil {
		return true, "no_approver", nil
	}
	return approver.Check(ctx, req)
}

func isBinarySample(sample []byte) bool {
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	return bytes.IndexByte(sample, 0) >= 0
}

// writeFileAtomic writes data to absPath atomically via a sibling temp file,
// falling back to direct os.WriteFile on Windows if rename fails (e.g., transient AV lock or symlink target).
func writeFileAtomic(absPath string, data []byte) error {
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	perm := os.FileMode(0o644)
	if fi, err := os.Stat(absPath); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory", absPath)
		}
		perm = fi.Mode().Perm()
		if perm == 0 {
			perm = 0o644
		}
	}

	tmp, err := os.CreateTemp(dir, ".coder-tmp-*")
	if err != nil {
		return os.WriteFile(absPath, data, perm)
	}
	tmpName := tmp.Name()

	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	_ = os.Chmod(tmpName, perm)

	if err := os.Rename(tmpName, absPath); err != nil {
		_ = os.Remove(tmpName)
		return os.WriteFile(absPath, data, perm)
	}
	return nil
}

// ignoreMatcher evaluates default ignored directory names plus basic root .gitignore directory rules.
type ignoreMatcher struct {
	workspaceRoot string
	dirNames      map[string]struct{}
	relDirPaths   map[string]struct{}
	allowNames    map[string]struct{}
}

func newIgnoreMatcher(workspaceDir, explicitTargetDir, pattern string) *ignoreMatcher {
	ws := normalizeWorkspaceDir(workspaceDir)
	m := &ignoreMatcher{
		workspaceRoot: ws,
		dirNames:      make(map[string]struct{}, len(defaultSkippedDirs)+8),
		relDirPaths:   make(map[string]struct{}),
		allowNames:    make(map[string]struct{}),
	}
	for k := range defaultSkippedDirs {
		m.dirNames[k] = struct{}{}
	}

	// Parse basic root .gitignore directory rules.
	if data, err := os.ReadFile(filepath.Join(ws, ".gitignore")); err == nil && len(data) <= 64*1024 {
		for _, rawLine := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
				continue
			}
			// Only treat explicit directory rules (trailing '/') or well-known simple directory entries without wildcards.
			if !strings.HasSuffix(line, "/") {
				continue
			}
			if strings.ContainsAny(line, "*?[") {
				continue
			}
			rooted := strings.HasPrefix(line, "/")
			cleaned := strings.Trim(line, "/")
			if cleaned == "" {
				continue
			}
			if runtime.GOOS == "windows" {
				cleaned = strings.ToLower(cleaned)
			}
			if rooted || strings.Contains(cleaned, "/") {
				m.relDirPaths[cleaned] = struct{}{}
			} else {
				m.dirNames[cleaned] = struct{}{}
			}
		}
	}

	// Do not skip directories explicitly targeted by path or leading pattern segment.
	if explicitTargetDir != "" {
		for _, seg := range strings.FieldsFunc(explicitTargetDir, func(r rune) bool { return r == '/' || r == '\\' }) {
			key := seg
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			m.allowNames[key] = struct{}{}
		}
	}
	if pattern != "" {
		normPat := normalizeGlobSlash(pattern)
		if idx := strings.IndexByte(normPat, '/'); idx > 0 {
			first := normPat[:idx]
			if !strings.ContainsAny(first, "*?[{") {
				if runtime.GOOS == "windows" {
					first = strings.ToLower(first)
				}
				m.allowNames[first] = struct{}{}
			}
		}
	}
	return m
}

func (m *ignoreMatcher) shouldSkipDir(absDirPath, dirName string) bool {
	nameKey := dirName
	if runtime.GOOS == "windows" {
		nameKey = strings.ToLower(nameKey)
	}
	if _, allowed := m.allowNames[nameKey]; allowed {
		return false
	}
	if _, skip := m.dirNames[nameKey]; skip {
		return true
	}
	if len(m.relDirPaths) > 0 && m.workspaceRoot != "" {
		if rel, err := filepath.Rel(m.workspaceRoot, absDirPath); err == nil {
			relSlash := filepath.ToSlash(rel)
			if runtime.GOOS == "windows" {
				relSlash = strings.ToLower(relSlash)
			}
			if _, skip := m.relDirPaths[relSlash]; skip {
				return true
			}
		}
	}
	return false
}

// headTailBuffer bounds captured command output in O(maxBytes) memory while preserving
// both the beginning (head) and the end (tail) of large outputs.
type headTailBuffer struct {
	mu       sync.Mutex
	maxBytes int
	headCap  int
	tailCap  int
	head     []byte
	tailRing []byte
	tailPos  int
	tailLen  int
	total    int64
}

func newHeadTailBuffer(maxBytes int) *headTailBuffer {
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutputBytes
	}
	if maxBytes < 64 {
		maxBytes = 64
	}
	headCap := maxBytes / 2
	tailCap := maxBytes - headCap
	return &headTailBuffer{
		maxBytes: maxBytes,
		headCap:  headCap,
		tailCap:  tailCap,
		head:     make([]byte, 0, min(headCap, 4096)),
	}
}

func (b *headTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	origLen := len(p)
	b.total += int64(origLen)

	if len(b.head) < b.headCap {
		need := b.headCap - len(b.head)
		if len(p) <= need {
			b.head = append(b.head, p...)
			return origLen, nil
		}
		b.head = append(b.head, p[:need]...)
		p = p[need:]
	}

	if b.tailCap <= 0 || len(p) == 0 {
		return origLen, nil
	}
	if b.tailRing == nil {
		b.tailRing = make([]byte, b.tailCap)
	}

	// If p alone exceeds tailCap, keep only the last tailCap bytes of p.
	if len(p) >= b.tailCap {
		copy(b.tailRing, p[len(p)-b.tailCap:])
		b.tailPos = 0
		b.tailLen = b.tailCap
		return origLen, nil
	}

	for len(p) > 0 {
		space := b.tailCap - b.tailPos
		n := min(space, len(p))
		copy(b.tailRing[b.tailPos:b.tailPos+n], p[:n])
		b.tailPos = (b.tailPos + n) % b.tailCap
		b.tailLen = min(b.tailCap, b.tailLen+n)
		p = p[n:]
	}
	return origLen, nil
}

func (b *headTailBuffer) TotalBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func (b *headTailBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total > int64(b.maxBytes)
}

func (b *headTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var tail []byte
	if b.tailLen > 0 {
		tail = make([]byte, b.tailLen)
		if b.tailLen < b.tailCap {
			copy(tail, b.tailRing[:b.tailLen])
		} else {
			first := b.tailCap - b.tailPos
			copy(tail, b.tailRing[b.tailPos:])
			copy(tail[first:], b.tailRing[:b.tailPos])
		}
	}

	if b.total <= int64(b.maxBytes) {
		combined := make([]byte, 0, len(b.head)+len(tail))
		combined = append(combined, b.head...)
		combined = append(combined, tail...)
		return string(combined)
	}

	omitted := b.total - int64(len(b.head)+len(tail))
	headStr := trimTrailingBrokenUTF8(b.head)
	tailStr := trimLeadingBrokenUTF8(tail)
	return fmt.Sprintf("%s\n\n... [%d bytes omitted from middle (%d total bytes)] ...\n\n%s",
		headStr, omitted, b.total, tailStr)
}

func trimTrailingBrokenUTF8(b []byte) string {
	for i := 0; i < 3 && len(b) > 0; i++ {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size != 1 {
			break
		}
		// Only trim continuation/incomplete multi-byte starter bytes (>= 0x80).
		if b[len(b)-1] < 0x80 {
			break
		}
		b = b[:len(b)-1]
	}
	return string(b)
}

func trimLeadingBrokenUTF8(b []byte) string {
	for i := 0; i < 3 && len(b) > 0; i++ {
		if (b[0] & 0xC0) != 0x80 {
			break
		}
		b = b[1:]
	}
	return string(b)
}
