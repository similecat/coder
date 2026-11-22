package customizations

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coder/internal/types"
)

const (
	skillFileName         = "SKILL.md"
	maxSkillDescRunes     = 160
	maxPluginWalkDepth    = 8
	maxPluginVisitedDirs  = 5000
	maxHelperFilesPerLoad = 500
)

var _ types.SkillCatalog = (*Catalog)(nil)

// Catalog implements types.SkillCatalog with eager compact metadata and lazy on-demand SKILL.md loading.
type Catalog struct {
	skills []types.SkillInfo
}

// NewCatalog creates a deterministic Catalog from discovered SkillInfo items.
func NewCatalog(skills []types.SkillInfo) *Catalog {
	copied := make([]types.SkillInfo, len(skills))
	copy(copied, skills)
	sortSkills(copied)
	return &Catalog{skills: copied}
}

// List returns all discovered skills sorted deterministically by name.
func (c *Catalog) List() []types.SkillInfo {
	if c == nil || len(c.skills) == 0 {
		return nil
	}
	out := make([]types.SkillInfo, len(c.skills))
	copy(out, c.skills)
	return out
}

// Load reads the full SKILL.md content on demand and lists relative paths of companion files in info.Dir.
func (c *Catalog) Load(name string) (content string, helperFiles []string, info types.SkillInfo, err error) {
	if c == nil {
		return "", nil, types.SkillInfo{}, errors.New("skill catalog is nil")
	}
	target := strings.TrimSpace(name)
	if target == "" {
		return "", nil, types.SkillInfo{}, errors.New("skill name is required")
	}

	matched, ok := c.findSkill(target)
	if !ok {
		return "", nil, types.SkillInfo{}, fmt.Errorf("skill %q not found", target)
	}

	raw, err := os.ReadFile(matched.SkillFile)
	if err != nil {
		return "", nil, types.SkillInfo{}, fmt.Errorf("read skill %q (%s): %w", matched.Name, matched.SkillFile, err)
	}

	_, body := ParseFrontmatter(string(raw))
	if body == "" {
		body = strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	}

	helpers, err := listSkillHelperFiles(matched.Dir, matched.SkillFile)
	if err != nil {
		return "", nil, types.SkillInfo{}, err
	}

	return body, helpers, matched, nil
}

func (c *Catalog) findSkill(name string) (types.SkillInfo, bool) {
	for _, s := range c.skills {
		if s.Name == name {
			return s, true
		}
	}
	for _, s := range c.skills {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return types.SkillInfo{}, false
}

// DiscoverSkills discovers SKILL.md metadata across native .coder and optional GitHub Copilot directories.
// Precedence on name collision (highest to lowest):
//  1. <workspaceDir>\.coder\skills\*\SKILL.md
//  2. <userHome>\.coder\skills\*\SKILL.md
//  3. <workspaceDir>\.github\skills\*\SKILL.md (when LoadCopilotSkills is true)
//  4. <workspaceDir>\.agents\skills\*\SKILL.md (when LoadCopilotSkills is true)
//  5. <userHome>\.copilot\skills\*\SKILL.md (when LoadCopilotSkills is true)
//  6. <userHome>\.copilot\installed-plugins\**\skills\*\SKILL.md (when LoadCopilotSkills is true)
func DiscoverSkills(workspaceDir string, userHome string, compat Compatibility) (*Catalog, error) {
	home := resolveUserHome(userHome)
	ws := strings.TrimSpace(workspaceDir)

	seenNames := make(map[string]struct{})
	seenFiles := make(map[string]struct{})
	var discovered []types.SkillInfo

	registerSkillDir := func(skillDir string, source types.InstructionSource) {
		skillPath := filepath.Join(skillDir, skillFileName)
		fileKey := canonicalPathKey(skillPath)
		if _, dupFile := seenFiles[fileKey]; dupFile {
			return
		}
		info, ok := readSkillMetadata(skillDir, skillPath, source)
		if !ok {
			return
		}
		seenFiles[fileKey] = struct{}{}
		nameKey := strings.ToLower(info.Name)
		if _, exists := seenNames[nameKey]; exists {
			return
		}
		seenNames[nameKey] = struct{}{}
		discovered = append(discovered, info)
	}

	scanSkillsRoot := func(skillsRoot string, source types.InstructionSource) {
		if skillsRoot == "" {
			return
		}
		entries, err := os.ReadDir(skillsRoot)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				continue
			}
			registerSkillDir(filepath.Join(skillsRoot, entry.Name()), source)
		}
	}

	// 1. Native workspace .coder/skills
	if ws != "" {
		scanSkillsRoot(filepath.Join(ws, ".coder", "skills"), types.SourceCoderRepo)
	}
	// 2. Native user ~/.coder/skills
	if home != "" {
		scanSkillsRoot(filepath.Join(home, ".coder", "skills"), types.SourceCoderUser)
	}

	// 3-6. Copilot-compatible skill roots
	if compat.LoadCopilotSkills {
		if ws != "" {
			scanSkillsRoot(filepath.Join(ws, ".github", "skills"), types.SourceCopilotRepo)
			scanSkillsRoot(filepath.Join(ws, ".agents", "skills"), types.SourceCopilotRepo)
		}
		if home != "" {
			scanSkillsRoot(filepath.Join(home, ".copilot", "skills"), types.SourceCopilotUser)
			scanInstalledPluginSkills(filepath.Join(home, ".copilot", "installed-plugins"), scanSkillsRoot)
		}
	}

	return NewCatalog(discovered), nil
}

func scanInstalledPluginSkills(pluginsRoot string, scanSkillsRoot func(string, types.InstructionSource)) {
	rootInfo, err := os.Lstat(pluginsRoot)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return
	}

	cleanRoot := filepath.Clean(pluginsRoot)
	visitedDirs := 0
	stopErr := errors.New("plugin walk limit reached")

	_ = filepath.WalkDir(cleanRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path != cleanRoot && d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}

		visitedDirs++
		if visitedDirs > maxPluginVisitedDirs {
			return stopErr
		}

		if path != cleanRoot {
			if shouldSkipWorkspaceDir(d.Name()) {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(cleanRoot, path)
			if err == nil {
				depth := strings.Count(filepath.ToSlash(rel), "/") + 1
				if depth > maxPluginWalkDepth {
					return filepath.SkipDir
				}
			}
			if strings.EqualFold(d.Name(), "skills") {
				scanSkillsRoot(path, types.SourceCopilotUser)
			}
		}
		return nil
	})
}

func readSkillMetadata(skillDir string, skillPath string, source types.InstructionSource) (types.SkillInfo, bool) {
	st, err := os.Lstat(skillPath)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || st.IsDir() {
		return types.SkillInfo{}, false
	}

	raw, err := os.ReadFile(skillPath)
	if err != nil {
		return types.SkillInfo{}, false
	}

	fm, body := ParseFrontmatter(string(raw))
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		name = filepath.Base(skillDir)
	}
	if name == "" || name == "." {
		return types.SkillInfo{}, false
	}

	desc := extractSkillDescription(fm, body)
	return types.SkillInfo{
		Name:        name,
		Description: desc,
		SkillFile:   filepath.Clean(skillPath),
		Dir:         filepath.Clean(skillDir),
		Source:      source,
	}, true
}

func extractSkillDescription(fm Frontmatter, body string) string {
	if d := collapseOneLine(fm.Description); d != "" {
		return truncateRunes(d, maxSkillDescRunes)
	}

	lines := strings.Split(body, "\n")
	inFence := false
	var para []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if len(para) > 0 {
				break
			}
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if trimmed == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#") ||
			trimmed == "---" || trimmed == "***" || trimmed == "___" ||
			(strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->")) {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, trimmed)
	}

	if len(para) == 0 {
		return ""
	}
	return truncateRunes(collapseOneLine(strings.Join(para, " ")), maxSkillDescRunes)
}

func collapseOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return strings.TrimRight(string(runes[:maxRunes-3]), " ") + "..."
}

func listSkillHelperFiles(skillDir string, skillFile string) ([]string, error) {
	if strings.TrimSpace(skillDir) == "" {
		return nil, nil
	}
	cleanDir := filepath.Clean(skillDir)
	cleanSkillFile := canonicalPathKey(skillFile)

	var helpers []string
	stopErr := errors.New("helper limit reached")
	err := filepath.WalkDir(cleanDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path != cleanDir && d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != cleanDir && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if canonicalPathKey(path) == cleanSkillFile {
			return nil
		}
		rel, relErr := filepath.Rel(cleanDir, path)
		if relErr != nil {
			return nil
		}
		if strings.EqualFold(rel, skillFileName) {
			return nil
		}
		helpers = append(helpers, filepath.ToSlash(rel))
		if len(helpers) >= maxHelperFilesPerLoad {
			return stopErr
		}
		return nil
	})
	if err != nil && !errors.Is(err, stopErr) {
		return nil, fmt.Errorf("walk skill dir %s: %w", cleanDir, err)
	}
	slices.Sort(helpers)
	return helpers, nil
}

func sortSkills(skills []types.SkillInfo) {
	slices.SortFunc(skills, func(a, b types.SkillInfo) int {
		la := strings.ToLower(a.Name)
		lb := strings.ToLower(b.Name)
		if c := strings.Compare(la, lb); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}
