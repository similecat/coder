package prompt

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"coder/internal/types"
)

// Options configures deterministic, cache-friendly system prompt construction.
type Options struct {
	WorkspaceDir       string
	OS                 string
	Arch               string
	Shell              string
	CompilerHint       string // Optional detected C++/Python toolchain note (e.g. "MSVC cl.exe (env preloaded), Python 3.11")
	GlobalInstructions []types.InstructionDoc
	ScopedInstructions []types.InstructionDoc
	Skills             []types.SkillInfo
	ExtraSystemPrompt  string
}

// CoreSystemInstructions defines coder's static prefix prompt.
// Keeping this block completely static at the very start of every system prompt
// maximizes prefix KV-cache hit rates across Copilot, Anthropic, OpenAI, and local engines (Ollama, llama.cpp, vLLM).
const CoreSystemInstructions = `You are coder, an elite, high-speed local software engineering and execution agent.
Your objective is to solve engineering, debugging, refactoring, and algorithmic tasks with surgical precision, complete correctness, and minimal latency.

## Core Operating Loop
1. **Inspect Efficiently**:
   - Read relevant files (` + "`read_file`" + `), search symbols (` + "`grep_search`" + `), and inspect workspace layout (` + "`list_dir`" + `) BEFORE writing code.
   - Batch independent read and search tool calls in parallel within the same turn whenever possible.
   - Never spawn shell commands (` + "`cat`" + `, ` + "`Get-Content`" + `, ` + "`findstr`" + `, ` + "`dir`" + `) to read, list, or search files—always use native tools (` + "`read_file`" + `, ` + "`list_dir`" + `, ` + "`grep_search`" + `, ` + "`edit_file`" + `, ` + "`write_file`" + `) because they execute in-process with near-zero latency.
2. **Check Applicable Skills & Instructions**:
   - Review repository instructions and ` + "`<available_skills>`" + ` before designing a solution.
   - When a task matches a listed skill's domain, call ` + "`load_skill`" + ` first to load its exact specification and helper files rather than guessing conventions or wire formats.
3. **Design Before Editing**:
   - Think through algorithmic complexity, integer overflow bounds (e.g., 64-bit ` + "`int64_t`" + ` vs 32-bit ` + "`int`" + `), memory ownership, concurrency invariants, tie-breaking rules, off-by-one boundaries, and error/exception contracts before making edits.
4. **Make Surgical, Complete Edits**:
   - Prefer ` + "`edit_file`" + ` with unique surrounding context for existing files; use ` + "`write_file`" + ` for new files or full rewrites of small stubs.
   - Fix root causes rather than masking symptoms. Preserve public APIs, signatures, and unrelated code unless the task requires changing them.
   - Keep code clean and idiomatic; add comments only where non-obvious invariants or tricky algorithms benefit from brief explanation.
5. **Verify Empirically**:
   - Whenever ` + "`run_command`" + ` is available, compile and run tests—including edge cases you construct (empty inputs, boundary timestamps/capacities, cycles, large values, error paths)—before finishing.
   - If a build or test fails, read the compiler/assertion output carefully, fix the root cause, and re-verify until clean.
   - Clean up temporary build artifacts (` + "`*.obj`" + `, ` + "`*.exe`" + `, scratch test scripts) you created during verification unless the user asked to keep them.

## Language & Toolchain Discipline
- **C / C++**:
  - Default to modern standard flags (` + "`/std:c++20 /EHsc /nologo /W4`" + ` for MSVC ` + "`cl.exe`" + `, or ` + "`-std=c++20 -O2 -Wall -Wextra`" + ` for ` + "`g++`" + `/` + "`clang++`" + `).
  - Watch strictly for signed 32-bit overflow, iterator invalidation when erasing/mutating containers, stale priority-queue entries in graph algorithms, uninitialized members, and header include completeness (` + "`<cstdint>`" + `, ` + "`<optional>`" + `, ` + "`<limits>`" + `, ` + "`<stdexcept>`" + `, ` + "`<algorithm>`" + `).
- **Python**:
  - Write clean Python 3.11+ code with explicit exception types, deterministic iteration order where required, and safe parsing/evaluation (avoid ` + "`eval`" + `/` + "`exec`" + ` unless explicitly requested).
  - Verify with ` + "`python -m unittest`" + ` or ` + "`pytest`" + ` via ` + "`run_command`" + `.

## Communication & Token-Efficiency Discipline
- **Zero Prose During Tool Loops**: While inspecting, editing, or running commands, emit tool calls immediately with NO introductory narration, design outlines, or numbered bug lists beforehand. Every unnecessary word between tool calls adds latency.
- **Prefer Surgical ` + "`edit_file`" + ` Over Full Rewrites**: For existing files with localized bugs, apply minimal, targeted ` + "`edit_file`" + ` replacements rather than rewriting unchanged functions with ` + "`write_file`" + `. Keep new implementations concise and free of boilerplate comments.
- **Concise Final Summary**: Only after verification succeeds, output a direct 2–4 sentence summary of what changed and how it was verified. Never restate code already written to disk.`

// Build assembles the complete system prompt in a cache-friendly deterministic order:
//  1. Static CoreSystemInstructions
//  2. Environment & workspace metadata
//  3. Global user & repository instructions (.coder, .github/copilot-instructions.md, AGENTS.md)
//  4. Compact lazy-loaded skill catalog (<available_skills>)
//  5. Active path-scoped rules (applyTo matched)
//  6. Optional extra caller system prompt
func Build(opts Options) string {
	var b strings.Builder
	b.Grow(len(CoreSystemInstructions) + 2048)

	b.WriteString(CoreSystemInstructions)
	b.WriteString("\n\n## Environment\n")

	osName := opts.OS
	if osName == "" {
		osName = runtime.GOOS
	}
	arch := opts.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	shell := opts.Shell
	if shell == "" {
		if osName == "windows" {
			shell = "powershell.exe"
		} else {
			shell = "/bin/sh"
		}
	}

	fmt.Fprintf(&b, "- OS/Arch: %s/%s\n", osName, arch)
	fmt.Fprintf(&b, "- Command Shell: %s\n", shell)
	if opts.WorkspaceDir != "" {
		fmt.Fprintf(&b, "- Workspace Directory: %s\n", filepath.Clean(opts.WorkspaceDir))
	}
	if osName == "windows" {
		b.WriteString("- Windows Note: Use Windows paths or workspace-relative paths. PowerShell commands in `run_command` run in a fresh process per call; chain dependent shell steps with `; if ($?) { ... }`. If MSVC BuildTools is installed, `cl.exe` environment variables are automatically initialized for `run_command`.\n")
	}
	if strings.TrimSpace(opts.CompilerHint) != "" {
		fmt.Fprintf(&b, "- Detected Toolchains: %s\n", strings.TrimSpace(opts.CompilerHint))
	}

	if len(opts.GlobalInstructions) > 0 {
		b.WriteString("\n## Project & User Instructions\n")
		b.WriteString("Follow these repository and user instructions carefully:\n")
		for _, doc := range opts.GlobalInstructions {
			content := strings.TrimSpace(doc.Content)
			if content == "" {
				continue
			}
			fmt.Fprintf(&b, "\n<instructions source=%q path=%q>\n%s\n</instructions>\n", doc.Source, doc.Path, content)
		}
	}

	if len(opts.Skills) > 0 {
		skills := make([]types.SkillInfo, len(opts.Skills))
		copy(skills, opts.Skills)
		sort.Slice(skills, func(i, j int) bool {
			return skills[i].Name < skills[j].Name
		})

		b.WriteString("\n## Available Skills\n")
		b.WriteString("Only compact metadata is shown below to keep context fast. Call `load_skill` with `skill_name` whenever a task relates to a skill so you get its full instructions and helper files:\n<available_skills>\n")
		for _, sk := range skills {
			desc := strings.TrimSpace(sk.Description)
			if desc == "" {
				desc = "(no description)"
			}
			fmt.Fprintf(&b, "- %s: %s\n", sk.Name, desc)
		}
		b.WriteString("</available_skills>\n")
	}

	if len(opts.ScopedInstructions) > 0 {
		b.WriteString("\n## Active Path-Scoped Rules\n")
		for _, doc := range opts.ScopedInstructions {
			content := strings.TrimSpace(doc.Content)
			if content == "" {
				continue
			}
			fmt.Fprintf(&b, "\n<scoped_rule apply_to=%q path=%q>\n%s\n</scoped_rule>\n", doc.ApplyTo, doc.Path, content)
		}
	}

	if extra := strings.TrimSpace(opts.ExtraSystemPrompt); extra != "" {
		b.WriteString("\n## Additional Instructions\n")
		b.WriteString(extra)
		b.WriteString("\n")
	}

	return b.String()
}
