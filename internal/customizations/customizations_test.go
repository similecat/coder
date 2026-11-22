package customizations

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"coder/internal/config"
	"coder/internal/types"
)

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseFrontmatter(t *testing.T) {
	t.Run("CRLF quoted applyTo and BOM", func(t *testing.T) {
		raw := "\xef\xbb\xbf---\r\nname: \"cpp-style\"\r\ndescription: \"Modern C++20 rules\"\r\napplyTo: \"**/*.cpp, **/*.hpp\"\r\n---\r\n\r\nPrefer `std::span` over raw pointers.\r\n"
		fm, body := ParseFrontmatter(raw)
		if fm.Name != "cpp-style" {
			t.Errorf("Name = %q, want %q", fm.Name, "cpp-style")
		}
		if fm.Description != "Modern C++20 rules" {
			t.Errorf("Description = %q, want %q", fm.Description, "Modern C++20 rules")
		}
		if fm.ApplyTo != "**/*.cpp, **/*.hpp" {
			t.Errorf("ApplyTo = %q, want %q", fm.ApplyTo, "**/*.cpp, **/*.hpp")
		}
		if body != "Prefer `std::span` over raw pointers." {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("inline list and single quotes", func(t *testing.T) {
		raw := "---\nname: 'header-guard'\ndescription: C# and C++ interop # trailing comment\napplyTo: [\"*.cpp\", '*.hpp', src/**/*.inl]\n---\nUse `#pragma once`.\n"
		fm, body := ParseFrontmatter(raw)
		if fm.Name != "header-guard" {
			t.Errorf("Name = %q", fm.Name)
		}
		if fm.Description != "C# and C++ interop" {
			t.Errorf("Description = %q, want 'C# and C++ interop'", fm.Description)
		}
		if fm.ApplyTo != "*.cpp, *.hpp, src/**/*.inl" {
			t.Errorf("ApplyTo = %q", fm.ApplyTo)
		}
		if body != "Use `#pragma once`." {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("multiline block scalar and YAML sequence", func(t *testing.T) {
		raw := "---\nname: cmake-helper\ndescription: >-\n  Configure and build CMake targets\n  with Ninja on Windows.\napplyTo:\n  - \"CMakeLists.txt\"\n  - \"**/*.cmake\"\n---\n# CMake\nRun cmake.\n"
		fm, body := ParseFrontmatter(raw)
		if fm.Name != "cmake-helper" {
			t.Errorf("Name = %q", fm.Name)
		}
		if fm.Description != "Configure and build CMake targets with Ninja on Windows." {
			t.Errorf("Description = %q", fm.Description)
		}
		if fm.ApplyTo != "CMakeLists.txt, **/*.cmake" {
			t.Errorf("ApplyTo = %q", fm.ApplyTo)
		}
		if body != "# CMake\nRun cmake." {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("no frontmatter with horizontal rule in body", func(t *testing.T) {
		raw := "# Project Guide\n\n---\n\nAlways run unit tests."
		fm, body := ParseFrontmatter(raw)
		if fm.Name != "" || fm.ApplyTo != "" || fm.Description != "" {
			t.Errorf("expected empty frontmatter, got %+v", fm)
		}
		if body != raw {
			t.Errorf("body = %q, want %q", body, raw)
		}
	})
}

func TestMatchApplyTo(t *testing.T) {
	cases := []struct {
		name     string
		applyTo  string
		relPaths []string
		want     bool
	}{
		{
			name:     "universal double star",
			applyTo:  "**",
			relPaths: []string{`deep\nested\dir\file.txt`},
			want:     true,
		},
		{
			name:     "double star cpp root and nested Windows backslashes",
			applyTo:  "**/*.cpp, **/*.hpp",
			relPaths: []string{`README.md`, `src\engine\math.cpp`},
			want:     true,
		},
		{
			name:     "double star cpp root file",
			applyTo:  "**/*.cpp",
			relPaths: []string{`main.cpp`},
			want:     true,
		},
		{
			name:     "nested prefix zero intermediate dirs",
			applyTo:  "src/**/*.py",
			relPaths: []string{`src\runner.py`},
			want:     true,
		},
		{
			name:     "nested prefix multiple intermediate dirs",
			applyTo:  "src/**/*.py",
			relPaths: []string{`.\src\pkg\sub\runner.py`},
			want:     true,
		},
		{
			name:     "nested prefix mismatch top dir",
			applyTo:  "src/**/*.py",
			relPaths: []string{`tests\runner.py`},
			want:     false,
		},
		{
			name:     "single star root match",
			applyTo:  "*.h",
			relPaths: []string{`version.h`},
			want:     true,
		},
		{
			name:     "single star does not cross subdir",
			applyTo:  "*.h",
			relPaths: []string{`include\version.h`},
			want:     false,
		},
		{
			name:     "brace expansion pattern",
			applyTo:  "**/*.{cpp,hpp}, CMakeLists.txt",
			relPaths: []string{`include\core\types.hpp`},
			want:     true,
		},
		{
			name:     "empty paths",
			applyTo:  "**/*.go",
			relPaths: nil,
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchApplyTo(tc.applyTo, tc.relPaths)
			if got != tc.want {
				t.Errorf("MatchApplyTo(%q, %v) = %v, want %v", tc.applyTo, tc.relPaths, got, tc.want)
			}
		})
	}
}

func TestDiscoverInstructionsCompatibilityAndScoping(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	// Native .coder instructions & rules
	writeFile(t, filepath.Join(home, ".coder", "instructions.md"), "Native user instructions")
	writeFile(t, filepath.Join(ws, ".coder", "instructions.md"), "Native repo instructions")
	writeFile(t, filepath.Join(ws, ".coder", "rules", "cpp.md"), "---\r\napplyTo: \"**/*.cpp, **/*.hpp\"\r\ndescription: \"C++ rule\"\r\n---\r\nUse RAII everywhere.")
	writeFile(t, filepath.Join(ws, ".coder", "rules", "all.md"), "---\napplyTo: \"**\"\n---\nUniversal coder rule.")

	// Copilot instructions (user + repo)
	writeFile(t, filepath.Join(home, ".copilot", "copilot-instructions.md"), "Copilot user global")
	writeFile(t, filepath.Join(home, ".copilot", "instructions", "python.instructions.md"), "---\napplyTo: \"src/**/*.py\"\n---\nUse type hints in Python.")
	writeFile(t, filepath.Join(ws, ".github", "copilot-instructions.md"), "Copilot repo global")
	writeFile(t, filepath.Join(ws, ".github", "instructions", "nested", "cmake.instructions.md"), "---\napplyTo: [\"CMakeLists.txt\", \"**/*.cmake\"]\n---\nTarget CMake 3.25+.")

	// AGENTS.md and CLAUDE.md
	writeFile(t, filepath.Join(ws, "AGENTS.md"), "Agents MD instructions")
	writeFile(t, filepath.Join(ws, "CLAUDE.md"), "Claude MD instructions")

	// Case 1: All compatibility enabled
	setAll, err := DiscoverInstructions(ws, home, config.DefaultCompatibility())
	if err != nil {
		t.Fatalf("DiscoverInstructions(all enabled) error: %v", err)
	}

	if len(setAll.GlobalInstructions) != 7 {
		t.Fatalf("GlobalInstructions count = %d, want 7; got %+v", len(setAll.GlobalInstructions), setAll.GlobalInstructions)
	}
	if len(setAll.ScopedInstructions) != 3 {
		t.Fatalf("ScopedInstructions count = %d, want 3; got %+v", len(setAll.ScopedInstructions), setAll.ScopedInstructions)
	}

	// Verify active scoped matching for C++ files only
	activeCpp := setAll.ActiveScopedInstructions([]string{`src\engine\solver.cpp`, `README.md`})
	if len(activeCpp) != 1 || activeCpp[0].Source != types.SourceCoderRepo || !strings.Contains(activeCpp[0].Content, "Use RAII") {
		t.Fatalf("ActiveScopedInstructions for C++ mismatch: %+v", activeCpp)
	}
	if strings.Contains(activeCpp[0].Content, "applyTo:") {
		t.Errorf("Frontmatter should be stripped from InstructionDoc.Content: %q", activeCpp[0].Content)
	}

	// Verify active scoped matching with absolute Windows paths inside workspace
	absPy := filepath.Join(ws, "src", "service", "api.py")
	activePyAndCMake := setAll.ActiveScopedInstructions([]string{absPy, `CMakeLists.txt`})
	if len(activePyAndCMake) != 2 {
		t.Fatalf("ActiveScopedInstructions for Python+CMake count = %d, want 2: %+v", len(activePyAndCMake), activePyAndCMake)
	}

	// Case 2: CODER_COPILOT_COMPAT=false + LoadAgentsMD=false
	t.Setenv("CODER_COPILOT_COMPAT", "false")
	cfgDisabled, err := config.Load(ws, home, config.Overrides{LoadAgentsMD: config.Bool(false)})
	if err != nil {
		t.Fatalf("config.Load error: %v", err)
	}

	setNativeOnly, err := DiscoverInstructions(ws, home, cfgDisabled.Compatibility)
	if err != nil {
		t.Fatalf("DiscoverInstructions(disabled compat) error: %v", err)
	}
	for _, doc := range setNativeOnly.All {
		if doc.Source != types.SourceCoderUser && doc.Source != types.SourceCoderRepo {
			t.Fatalf("unexpected non-coder instruction loaded when compat=false: %+v", doc)
		}
	}
	if len(setNativeOnly.GlobalInstructions) != 3 {
		t.Fatalf("native GlobalInstructions count = %d, want 3", len(setNativeOnly.GlobalInstructions))
	}
	if len(setNativeOnly.ScopedInstructions) != 1 {
		t.Fatalf("native ScopedInstructions count = %d, want 1", len(setNativeOnly.ScopedInstructions))
	}
}

func TestDiscoverSkillsPrecedenceCompatAndLazyLoad(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	// Colliding skill "foo" in .coder/skills, ~/.coder/skills, and .github/skills
	coderFooDir := filepath.Join(ws, ".coder", "skills", "foo")
	writeFile(t, filepath.Join(coderFooDir, "SKILL.md"), "---\r\nname: foo\r\ndescription: Native repo foo skill\r\n---\r\n# Native Foo\r\nUse helper script.\r\n")
	writeFile(t, filepath.Join(coderFooDir, "scripts", "verify.py"), "print('ok')\n")
	writeFile(t, filepath.Join(coderFooDir, "reference.md"), "Reference notes\n")

	writeFile(t, filepath.Join(home, ".coder", "skills", "foo", "SKILL.md"), "---\nname: foo\ndescription: User coder foo\n---\nUser coder body")
	writeFile(t, filepath.Join(ws, ".github", "skills", "foo", "SKILL.md"), "---\nname: foo\ndescription: GitHub repo foo\n---\nGitHub body")

	// Fallback folder name & long paragraph description truncation (>160 chars)
	longPara := strings.Repeat("build-and-verify-msvc ", 10)
	writeFile(t, filepath.Join(ws, ".github", "skills", "msvc-check", "SKILL.md"), "# Heading Ignored\n\n"+longPara+"\n\nSecond paragraph ignored.")

	// .agents/skills, ~/.copilot/skills, and ~/.copilot/installed-plugins/**/skills/*
	writeFile(t, filepath.Join(ws, ".agents", "skills", "agent-lint", "SKILL.md"), "---\ndescription: Agent lint helper\n---\nLint steps")
	writeFile(t, filepath.Join(home, ".copilot", "skills", "user-copilot", "SKILL.md"), "---\nname: user-copilot\ndescription: User copilot skill\n---\nBody")
	writeFile(t, filepath.Join(home, ".copilot", "installed-plugins", "publisher", "bundle-v1", "skills", "plugin-debug", "SKILL.md"), "---\nname: plugin-debug\ndescription: Plugin debugger skill\n---\nPlugin body")

	// Discover with LoadCopilotSkills = true
	cat, err := DiscoverSkills(ws, home, Compatibility{LoadCopilotSkills: true})
	if err != nil {
		t.Fatalf("DiscoverSkills(true) error: %v", err)
	}

	listed := cat.List()
	var gotNames []string
	for _, s := range listed {
		gotNames = append(gotNames, s.Name)
	}
	wantNames := []string{"agent-lint", "foo", "msvc-check", "plugin-debug", "user-copilot"}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("List() names = %v, want %v", gotNames, wantNames)
	}

	// Verify .coder/skills/foo won collision over ~/.coder and .github
	_, _, fooMeta, err := cat.Load("foo")
	if err != nil {
		t.Fatalf("Load(foo) error: %v", err)
	}
	if fooMeta.Source != types.SourceCoderRepo || fooMeta.Description != "Native repo foo skill" {
		t.Fatalf("foo precedence mismatch: %+v", fooMeta)
	}

	// Verify msvc-check fallback name and <=160 rune 1-line description
	_, _, msvcMeta, err := cat.Load("msvc-check")
	if err != nil {
		t.Fatalf("Load(msvc-check) error: %v", err)
	}
	if len([]rune(msvcMeta.Description)) > 160 || !strings.HasSuffix(msvcMeta.Description, "...") || strings.Contains(msvcMeta.Description, "\n") {
		t.Fatalf("expected <=160 char truncated 1-line description, got (%d runes): %q", len([]rune(msvcMeta.Description)), msvcMeta.Description)
	}

	// Verify lazy on-demand disk read + case-insensitive lookup + companion helper files
	writeFile(t, filepath.Join(coderFooDir, "SKILL.md"), "---\nname: foo\ndescription: Native repo foo skill\n---\n# Updated Lazy Content\nStep 1: run scripts/verify.py")
	content, helpers, info, err := cat.Load("FOO")
	if err != nil {
		t.Fatalf("Load(FOO) case-insensitive failed: %v", err)
	}
	if !strings.Contains(content, "# Updated Lazy Content") {
		t.Errorf("expected lazy on-demand content read, got %q", content)
	}
	wantHelpers := []string{"reference.md", "scripts/verify.py"}
	if !slices.Equal(helpers, wantHelpers) {
		t.Errorf("helperFiles = %v, want %v", helpers, wantHelpers)
	}
	if info.Dir != filepath.Clean(coderFooDir) {
		t.Errorf("info.Dir = %q, want %q", info.Dir, filepath.Clean(coderFooDir))
	}

	// Discover with LoadCopilotSkills = false
	catNativeOnly, err := DiscoverSkills(ws, home, Compatibility{LoadCopilotSkills: false})
	if err != nil {
		t.Fatalf("DiscoverSkills(false) error: %v", err)
	}
	nativeList := catNativeOnly.List()
	if len(nativeList) != 1 || nativeList[0].Name != "foo" || nativeList[0].Source != types.SourceCoderRepo {
		t.Fatalf("expected only native foo skill when LoadCopilotSkills=false, got %+v", nativeList)
	}
}

func TestDiscoverCustomTools(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	// User tools file (JSON array)
	userToolsPath := filepath.Join(home, ".coder", "tools", "base.json")
	writeFile(t, userToolsPath, `[
  {
    "name": "run_lint",
    "description": "User lint",
    "command": "ruff",
    "args": ["check", "."]
  },
  {
    "name": "git_summary",
    "description": "Summarize repo",
    "command": "git",
    "args": ["status", "-s"],
    "read_only": true
  }
]`)

	// Workspace tool file (single JSON object overriding run_lint, nil parameters)
	wsToolsPath := filepath.Join(ws, ".coder", "tools", "lint.json")
	writeFile(t, wsToolsPath, `{
  "name": "run_lint",
  "description": "Workspace clang-tidy lint",
  "command": "clang-tidy"
}`)

	tools, err := DiscoverCustomTools(ws, home)
	if err != nil {
		t.Fatalf("DiscoverCustomTools error: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2: %+v", len(tools), tools)
	}

	// Sorted deterministically: git_summary, run_lint
	if tools[0].Name != "git_summary" || !tools[0].ReadOnly {
		t.Errorf("unexpected tool[0]: %+v", tools[0])
	}
	if tools[1].Name != "run_lint" || tools[1].Command != "clang-tidy" || tools[1].SourceFile != filepath.Clean(wsToolsPath) {
		t.Errorf("workspace tool should override user tool on same Name, got %+v", tools[1])
	}
	if tools[1].Parameters == nil || tools[1].Parameters["type"] != "object" {
		t.Errorf("nil Parameters should default to object schema, got %+v", tools[1].Parameters)
	}

	// Validation failure when command is missing
	badWS := t.TempDir()
	writeFile(t, filepath.Join(badWS, ".coder", "tools", "bad.json"), `{"name": "broken_tool"}`)
	if _, err := DiscoverCustomTools(badWS, home); err == nil {
		t.Fatalf("expected validation error for missing command")
	}
}

func TestLoadAllAndCollectWorkspaceFiles(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	writeFile(t, filepath.Join(ws, ".coder", "instructions.md"), "Repo global instruction")
	writeFile(t, filepath.Join(ws, ".coder", "rules", "cpp.md"), "---\napplyTo: \"src/**/*.cpp\"\n---\nScoped C++ instruction")
	writeFile(t, filepath.Join(ws, ".coder", "skills", "bench", "SKILL.md"), "---\ndescription: Benchmark runner\n---\nRun bench")
	writeFile(t, filepath.Join(ws, ".coder", "tools", "build.json"), `{"name":"build_cpp","command":"cl.exe"}`)
	writeFile(t, filepath.Join(ws, "src", "solver.cpp"), "int main() { return 0; }\n")
	writeFile(t, filepath.Join(ws, "node_modules", "ignored", "junk.cpp"), "// skipped\n")

	files, err := CollectWorkspaceFiles(ws, 100)
	if err != nil {
		t.Fatalf("CollectWorkspaceFiles error: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f, "node_modules/") {
			t.Fatalf("CollectWorkspaceFiles should skip node_modules, got %q", f)
		}
	}

	bundle, err := LoadAll(ws, home, config.DefaultCompatibility())
	if err != nil {
		t.Fatalf("LoadAll error: %v", err)
	}
	active := bundle.Instructions.ActiveInstructions(files)
	if len(active) != 2 {
		t.Fatalf("ActiveInstructions count = %d, want 2: %+v", len(active), active)
	}
	if len(bundle.Skills.List()) != 1 || len(bundle.CustomTools) != 1 {
		t.Fatalf("unexpected bundle contents: skills=%d tools=%d", len(bundle.Skills.List()), len(bundle.CustomTools))
	}
}

