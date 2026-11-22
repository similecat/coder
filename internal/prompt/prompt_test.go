package prompt

import (
	"strings"
	"testing"

	"coder/internal/types"
)

func TestBuildPromptDeterministicAndCacheFriendly(t *testing.T) {
	opts := Options{
		WorkspaceDir: `D:\work\sample`,
		OS:           "windows",
		Arch:         "amd64",
		CompilerHint: "MSVC cl.exe, Python 3.11",
		GlobalInstructions: []types.InstructionDoc{
			{
				Path:    ".github/copilot-instructions.md",
				Source:  types.SourceCopilotRepo,
				Content: "Always use snake_case for telemetry fields.",
			},
			{
				Path:    ".coder/instructions.md",
				Source:  types.SourceCoderRepo,
				Content: "Prefer std::span in C++20 interfaces.",
			},
		},
		Skills: []types.SkillInfo{
			{Name: "zeta-skill", Description: "Zeta helper"},
			{Name: "alpha-skill", Description: "Alpha packet rules"},
		},
		ScopedInstructions: []types.InstructionDoc{
			{
				Path:    ".github/instructions/cpp.instructions.md",
				Source:  types.SourceCopilotRepo,
				ApplyTo: "**/*.cpp",
				Content: "Compile with /W4 and zero warnings.",
			},
		},
	}

	out1 := Build(opts)
	out2 := Build(opts)
	if out1 != out2 {
		t.Fatalf("expected deterministic prompt output across calls")
	}

	if !strings.HasPrefix(out1, CoreSystemInstructions) {
		t.Fatalf("expected CoreSystemInstructions to remain static prefix for KV-cache reuse")
	}

	coreIdx := strings.Index(out1, "## Core Operating Loop")
	envIdx := strings.Index(out1, "## Environment")
	instIdx := strings.Index(out1, "## Project & User Instructions")
	skillIdx := strings.Index(out1, "## Available Skills")
	scopedIdx := strings.Index(out1, "## Active Path-Scoped Rules")

	if !(coreIdx < envIdx && envIdx < instIdx && instIdx < skillIdx && skillIdx < scopedIdx) {
		t.Fatalf("unexpected section ordering: core=%d env=%d inst=%d skill=%d scoped=%d",
			coreIdx, envIdx, instIdx, skillIdx, scopedIdx)
	}

	// Verify skills are sorted alphabetically (alpha-skill before zeta-skill).
	alphaIdx := strings.Index(out1, "- alpha-skill: Alpha packet rules")
	zetaIdx := strings.Index(out1, "- zeta-skill: Zeta helper")
	if alphaIdx < 0 || zetaIdx < 0 || alphaIdx >= zetaIdx {
		t.Fatalf("expected skills sorted deterministically: alpha=%d zeta=%d", alphaIdx, zetaIdx)
	}
}
