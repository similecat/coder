package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const defaultProblemTimeoutSec = 180

// Problem describes a single self-contained coding benchmark task and its host verifier.
type Problem struct {
	ID                    string   `json:"id"`
	Language              string   `json:"language"`
	Title                 string   `json:"title"`
	Prompt                string   `json:"prompt"`
	TimeoutSec            int      `json:"timeout_sec"`
	CopilotCompatRequired bool     `json:"copilot_compat_required"`
	VerifyEntry           string   `json:"verify_entry"`
	CPPSources            []string `json:"cpp_sources,omitempty"`
	Dir                   string   `json:"dir"`
	WorkspaceDir          string   `json:"workspace_dir"`
}

// ResolveProblemsRoot locates the benchmark problems root directory.
// When problemsRoot is empty, it searches upward from the current working directory
// and executable directory for testdata\problems.
func ResolveProblemsRoot(problemsRoot string) (string, error) {
	if trimmed := strings.TrimSpace(problemsRoot); trimmed != "" {
		abs, err := filepath.Abs(filepath.Clean(trimmed))
		if err != nil {
			return "", fmt.Errorf("resolve problems dir %q: %w", trimmed, err)
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("stat problems dir %s: %w", abs, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("problems path %s is not a directory", abs)
		}
		return abs, nil
	}

	var candidates []string
	seen := make(map[string]struct{})
	addCandidate := func(p string) {
		if p == "" {
			return
		}
		clean, err := filepath.Abs(filepath.Clean(p))
		if err != nil {
			return
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, clean)
	}

	addAncestorCandidates := func(startDir string) {
		dir := filepath.Clean(startDir)
		for i := 0; i < 10 && dir != ""; i++ {
			addCandidate(filepath.Join(dir, "testdata", "problems"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		addAncestorCandidates(cwd)
	}
	if exePath, err := os.Executable(); err == nil && exePath != "" {
		exeDir := filepath.Dir(exePath)
		addAncestorCandidates(exeDir)
		addCandidate(filepath.Join(exeDir, "problems"))
	}

	for _, cand := range candidates {
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand, nil
		}
	}

	return "", fmt.Errorf("could not auto-locate testdata\\problems (searched %d paths; pass --problems-dir)", len(candidates))
}

// LoadProblems discovers and parses benchmark problems under problemsRoot.
// If filterIDs is non-empty (supporting individual or comma-separated IDs), only matching
// problems are returned in the requested order.
func LoadProblems(problemsRoot string, filterIDs []string) ([]Problem, error) {
	root, err := ResolveProblemsRoot(problemsRoot)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read problems root %s: %w", root, err)
	}

	var all []Problem
	byID := make(map[string]Problem, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		probDir := filepath.Join(root, entry.Name())
		manifestPath := filepath.Join(probDir, "problem.json")
		if fi, statErr := os.Stat(manifestPath); statErr != nil || fi.IsDir() {
			continue
		}

		prob, parseErr := loadSingleProblem(probDir, manifestPath)
		if parseErr != nil {
			return nil, parseErr
		}
		key := strings.ToLower(prob.ID)
		if _, dup := byID[key]; dup {
			return nil, fmt.Errorf("duplicate benchmark problem ID %q in %s", prob.ID, probDir)
		}
		byID[key] = prob
		all = append(all, prob)
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no benchmark problems found under %s", root)
	}

	slices.SortFunc(all, func(a, b Problem) int {
		return strings.Compare(a.ID, b.ID)
	})

	requested := normalizeFilterIDs(filterIDs)
	if len(requested) == 0 {
		return all, nil
	}

	filtered := make([]Problem, 0, len(requested))
	for _, id := range requested {
		prob, ok := byID[strings.ToLower(id)]
		if !ok {
			avail := make([]string, len(all))
			for i, p := range all {
				avail[i] = p.ID
			}
			return nil, fmt.Errorf("unknown benchmark problem %q (available: %s)", id, strings.Join(avail, ", "))
		}
		filtered = append(filtered, prob)
	}

	return filtered, nil
}

// FilterProblemsByLanguage filters a problem slice by language ("cpp" or "python").
// When lang is empty, all input problems are returned unchanged.
func FilterProblemsByLanguage(problems []Problem, lang string) ([]Problem, error) {
	trimmed := strings.TrimSpace(lang)
	if trimmed == "" {
		return problems, nil
	}
	norm, err := normalizeLanguage(trimmed)
	if err != nil {
		return nil, err
	}
	out := make([]Problem, 0, len(problems))
	for _, p := range problems {
		if p.Language == norm {
			out = append(out, p)
		}
	}
	return out, nil
}

func loadSingleProblem(probDir, manifestPath string) (Problem, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Problem{}, fmt.Errorf("read %s: %w", manifestPath, err)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	var prob Problem
	if err := json.Unmarshal(raw, &prob); err != nil {
		return Problem{}, fmt.Errorf("parse %s: %w", manifestPath, err)
	}

	prob.ID = strings.TrimSpace(prob.ID)
	prob.Title = strings.TrimSpace(prob.Title)
	prob.Prompt = strings.TrimSpace(prob.Prompt)
	prob.VerifyEntry = strings.TrimSpace(prob.VerifyEntry)

	if prob.ID == "" {
		return Problem{}, fmt.Errorf("%s: missing required field \"id\"", manifestPath)
	}
	normLang, err := normalizeLanguage(prob.Language)
	if err != nil {
		return Problem{}, fmt.Errorf("%s (%s): %w", manifestPath, prob.ID, err)
	}
	prob.Language = normLang

	if prob.Title == "" {
		return Problem{}, fmt.Errorf("%s (%s): missing required field \"title\"", manifestPath, prob.ID)
	}
	if prob.Prompt == "" {
		return Problem{}, fmt.Errorf("%s (%s): missing required field \"prompt\"", manifestPath, prob.ID)
	}
	if prob.VerifyEntry == "" {
		return Problem{}, fmt.Errorf("%s (%s): missing required field \"verify_entry\"", manifestPath, prob.ID)
	}
	if prob.TimeoutSec <= 0 {
		prob.TimeoutSec = defaultProblemTimeoutSec
	}

	absDir, err := filepath.Abs(filepath.Clean(probDir))
	if err != nil {
		return Problem{}, fmt.Errorf("resolve problem dir %s: %w", probDir, err)
	}
	wsDir := filepath.Join(absDir, "workspace")
	if fi, statErr := os.Stat(wsDir); statErr != nil || !fi.IsDir() {
		return Problem{}, fmt.Errorf("%s (%s): workspace directory missing at %s", manifestPath, prob.ID, wsDir)
	}

	verifyPath := filepath.Join(absDir, filepath.FromSlash(prob.VerifyEntry))
	if fi, statErr := os.Stat(verifyPath); statErr != nil || fi.IsDir() {
		return Problem{}, fmt.Errorf("%s (%s): verify_entry missing at %s", manifestPath, prob.ID, verifyPath)
	}

	prob.Dir = absDir
	prob.WorkspaceDir = wsDir
	return prob, nil
}

func normalizeLanguage(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cpp", "c++", "cxx":
		return "cpp", nil
	case "python", "py":
		return "python", nil
	default:
		return "", fmt.Errorf("unsupported language %q (expected \"cpp\" or \"python\")", raw)
	}
}

func normalizeFilterIDs(rawIDs []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, entry := range rawIDs {
		for _, part := range strings.Split(entry, ",") {
			id := strings.TrimSpace(part)
			if id == "" {
				continue
			}
			key := strings.ToLower(id)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// CopyWorkspace recursively copies srcDir into dstDir, preserving hidden configuration
// directories such as .github, .coder, and .agents while skipping transient __pycache__/.git dirs.
func CopyWorkspace(srcDir, dstDir string) error {
	cleanSrc, err := filepath.Abs(filepath.Clean(srcDir))
	if err != nil {
		return fmt.Errorf("resolve source workspace %s: %w", srcDir, err)
	}
	cleanDst, err := filepath.Abs(filepath.Clean(dstDir))
	if err != nil {
		return fmt.Errorf("resolve destination workspace %s: %w", dstDir, err)
	}

	srcInfo, err := os.Stat(cleanSrc)
	if err != nil {
		return fmt.Errorf("stat source workspace %s: %w", cleanSrc, err)
	}
	if !srcInfo.IsDir() {
		return errors.New("source workspace is not a directory")
	}

	if err := os.MkdirAll(cleanDst, 0o755); err != nil {
		return fmt.Errorf("create destination workspace %s: %w", cleanDst, err)
	}

	return filepath.WalkDir(cleanSrc, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == cleanSrc {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		nameLower := strings.ToLower(d.Name())
		if d.IsDir() && (nameLower == "__pycache__" || nameLower == ".pytest_cache" || nameLower == ".git") {
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(cleanSrc, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(cleanDst, rel)

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		return copyRegularFile(path, targetPath)
	})
}

func copyRegularFile(srcFile, dstFile string) error {
	if err := os.MkdirAll(filepath.Dir(dstFile), 0o755); err != nil {
		return err
	}

	in, err := os.Open(srcFile)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcFile, err)
	}
	defer in.Close()

	mode := fs.FileMode(0o644)
	if fi, statErr := in.Stat(); statErr == nil {
		mode = fi.Mode().Perm() | 0o600
	}

	out, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dstFile, err)
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %s -> %s: %w", srcFile, dstFile, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", dstFile, closeErr)
	}
	return nil
}
