package customizations

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coder/internal/config"
	"coder/internal/types"
)

// Compatibility aliases config.Compatibility so callers can pass cfg.Compatibility directly.
type Compatibility = config.Compatibility

// Bundle groups all discovered workspace and user customizations.
type Bundle struct {
	Instructions *InstructionSet        `json:"instructions"`
	Skills       *Catalog               `json:"skills"`
	CustomTools  []types.CustomToolSpec `json:"custom_tools"`
}

// LoadAll discovers instructions, skills, and custom tools for the given workspace and user home.
func LoadAll(workspaceDir string, userHome string, compat Compatibility) (*Bundle, error) {
	inst, err := DiscoverInstructions(workspaceDir, userHome, compat)
	if err != nil {
		return nil, err
	}
	skills, err := DiscoverSkills(workspaceDir, userHome, compat)
	if err != nil {
		return nil, err
	}
	customTools, err := DiscoverCustomTools(workspaceDir, userHome)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		Instructions: inst,
		Skills:       skills,
		CustomTools:  customTools,
	}, nil
}

// InstructionSet separates discovered instruction files into always-active global instructions
// and path-scoped instructions evaluated dynamically against workspace/context files.
type InstructionSet struct {
	All                []types.InstructionDoc `json:"all"`
	GlobalInstructions []types.InstructionDoc `json:"global_instructions"`
	ScopedInstructions []types.InstructionDoc `json:"scoped_instructions"`
	workspaceDir       string
}

// ActiveScopedInstructions returns all ScopedInstructions whose ApplyTo pattern matches
// at least one file in workspaceFiles (accepting either workspace-relative or absolute paths).
func (s *InstructionSet) ActiveScopedInstructions(workspaceFiles []string) []types.InstructionDoc {
	if s == nil || len(s.ScopedInstructions) == 0 || len(workspaceFiles) == 0 {
		return nil
	}
	relPaths := s.toRelativePaths(workspaceFiles)
	if len(relPaths) == 0 {
		return nil
	}
	var active []types.InstructionDoc
	for _, doc := range s.ScopedInstructions {
		if MatchApplyTo(doc.ApplyTo, relPaths) {
			active = append(active, doc)
		}
	}
	return active
}

// ActiveInstructions returns GlobalInstructions followed by matching ScopedInstructions.
func (s *InstructionSet) ActiveInstructions(workspaceFiles []string) []types.InstructionDoc {
	if s == nil {
		return nil
	}
	scoped := s.ActiveScopedInstructions(workspaceFiles)
	out := make([]types.InstructionDoc, 0, len(s.GlobalInstructions)+len(scoped))
	out = append(out, s.GlobalInstructions...)
	out = append(out, scoped...)
	return out
}

func (s *InstructionSet) toRelativePaths(files []string) []string {
	ws := strings.TrimSpace(s.workspaceDir)
	if ws != "" {
		ws = filepath.Clean(ws)
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if ws != "" && filepath.IsAbs(f) {
			if rel, err := filepath.Rel(ws, f); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				out = append(out, rel)
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// DiscoverInstructions loads global and path-scoped instructions from native .coder directories
// and optional GitHub Copilot / AGENTS.md locations according to compat settings.
func DiscoverInstructions(workspaceDir string, userHome string, compat Compatibility) (*InstructionSet, error) {
	home := resolveUserHome(userHome)
	ws := strings.TrimSpace(workspaceDir)

	set := &InstructionSet{
		workspaceDir: ws,
	}
	seen := make(map[string]struct{})

	appendDoc := func(doc types.InstructionDoc) {
		set.All = append(set.All, doc)
		if IsGlobalApplyTo(doc.ApplyTo) {
			set.GlobalInstructions = append(set.GlobalInstructions, doc)
		} else {
			set.ScopedInstructions = append(set.ScopedInstructions, doc)
		}
	}

	loadSingle := func(path string, source types.InstructionSource) error {
		if path == "" {
			return nil
		}
		clean := filepath.Clean(path)
		key := canonicalPathKey(clean)
		if _, ok := seen[key]; ok {
			return nil
		}
		doc, ok, err := readInstructionFile(clean, source)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		seen[key] = struct{}{}
		appendDoc(doc)
		return nil
	}

	loadMatchingDir := func(rootDir string, suffix string, source types.InstructionSource) error {
		if rootDir == "" {
			return nil
		}
		files, err := collectMarkdownFiles(rootDir, suffix)
		if err != nil {
			return err
		}
		for _, f := range files {
			if err := loadSingle(f, source); err != nil {
				return err
			}
		}
		return nil
	}

	// 1. User-level Copilot instructions (optional)
	if compat.LoadCopilotInstructions && home != "" {
		if err := loadSingle(filepath.Join(home, ".copilot", "copilot-instructions.md"), types.SourceCopilotUser); err != nil {
			return nil, err
		}
		if err := loadMatchingDir(filepath.Join(home, ".copilot", "instructions"), ".instructions.md", types.SourceCopilotUser); err != nil {
			return nil, err
		}
	}

	// 2. User-level native .coder instructions (always active)
	if home != "" {
		if err := loadSingle(filepath.Join(home, ".coder", "instructions.md"), types.SourceCoderUser); err != nil {
			return nil, err
		}
		if err := loadMatchingDir(filepath.Join(home, ".coder", "rules"), ".md", types.SourceCoderUser); err != nil {
			return nil, err
		}
	}

	if ws != "" {
		// 3. AGENTS.md / CLAUDE.md (optional)
		if compat.LoadAgentsMD {
			if err := loadSingle(filepath.Join(ws, "AGENTS.md"), types.SourceAgentsMD); err != nil {
				return nil, err
			}
			if err := loadSingle(filepath.Join(ws, "CLAUDE.md"), types.SourceAgentsMD); err != nil {
				return nil, err
			}
		}

		// 4. Repo-level Copilot instructions (optional)
		if compat.LoadCopilotInstructions {
			if err := loadSingle(filepath.Join(ws, ".github", "copilot-instructions.md"), types.SourceCopilotRepo); err != nil {
				return nil, err
			}
			if err := loadMatchingDir(filepath.Join(ws, ".github", "instructions"), ".instructions.md", types.SourceCopilotRepo); err != nil {
				return nil, err
			}
		}

		// 5. Repo-level native .coder instructions & rules (always active)
		if err := loadSingle(filepath.Join(ws, ".coder", "instructions.md"), types.SourceCoderRepo); err != nil {
			return nil, err
		}
		if err := loadMatchingDir(filepath.Join(ws, ".coder", "rules"), ".md", types.SourceCoderRepo); err != nil {
			return nil, err
		}
	}

	return set, nil
}

func readInstructionFile(path string, source types.InstructionSource) (types.InstructionDoc, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return types.InstructionDoc{}, false, nil
		}
		return types.InstructionDoc{}, false, fmt.Errorf("stat instruction file %s: %w", path, err)
	}
	if info.IsDir() {
		return types.InstructionDoc{}, false, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return types.InstructionDoc{}, false, fmt.Errorf("read instruction file %s: %w", path, err)
	}
	fm, body := ParseFrontmatter(string(raw))
	if strings.TrimSpace(body) == "" {
		return types.InstructionDoc{}, false, nil
	}
	return types.InstructionDoc{
		Path:        path,
		Source:      source,
		ApplyTo:     strings.TrimSpace(fm.ApplyTo),
		Description: strings.TrimSpace(fm.Description),
		Content:     body,
	}, true, nil
}

func collectMarkdownFiles(rootDir string, lowerSuffix string) ([]string, error) {
	info, err := os.Lstat(rootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat instructions dir %s: %w", rootDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil
	}

	var files []string
	err = filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path != rootDir && d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) && path != rootDir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), lowerSuffix) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk instructions dir %s: %w", rootDir, err)
	}

	slices.SortFunc(files, func(a, b string) int {
		ra, _ := filepath.Rel(rootDir, a)
		rb, _ := filepath.Rel(rootDir, b)
		return strings.Compare(filepath.ToSlash(ra), filepath.ToSlash(rb))
	})
	return files, nil
}

// CollectWorkspaceFiles walks workspaceDir up to maxFiles (default 5000 if <= 0),
// returning sorted slash-normalized relative file paths suitable for ActiveScopedInstructions.
func CollectWorkspaceFiles(workspaceDir string, maxFiles int) ([]string, error) {
	ws := strings.TrimSpace(workspaceDir)
	if ws == "" {
		return nil, nil
	}
	if maxFiles <= 0 {
		maxFiles = 5000
	}
	info, err := os.Stat(ws)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat workspace %s: %w", ws, err)
	}
	if !info.IsDir() {
		return nil, nil
	}

	stopWalk := errors.New("max files reached")
	var rels []string
	err = filepath.WalkDir(ws, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path != ws && d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != ws && shouldSkipWorkspaceDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(ws, path)
		if relErr == nil {
			rels = append(rels, filepath.ToSlash(rel))
			if len(rels) >= maxFiles {
				return stopWalk
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, stopWalk) {
		return nil, err
	}
	slices.Sort(rels)
	return rels, nil
}

func resolveUserHome(userHome string) string {
	h := strings.TrimSpace(userHome)
	if h != "" {
		return h
	}
	resolved, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return resolved
}

func canonicalPathKey(path string) string {
	return strings.ToLower(filepath.Clean(path))
}

func shouldSkipDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", "node_modules", "__pycache__", ".venv", "venv":
		return true
	default:
		return false
	}
}

func shouldSkipWorkspaceDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build", "target", "out", "__pycache__", ".venv", "venv", ".pytest_cache", ".mypy_cache":
		return true
	default:
		return false
	}
}
