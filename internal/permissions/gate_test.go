package permissions

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"coder/internal/types"
)

func TestIsWithinWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "workspace_sibling")
	if err := os.MkdirAll(filepath.Join(ws, "src", "nested"), 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}

	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{name: "dot", target: ".", want: true},
		{name: "relative_file", target: filepath.Join("src", "main.go"), want: true},
		{name: "relative_nonexistent_deep", target: filepath.Join("src", "newpkg", "file.go"), want: true},
		{name: "clean_internal_dotdot", target: filepath.Join("src", "nested", "..", "ok.go"), want: true},
		{name: "absolute_inside", target: filepath.Join(ws, "src", "main.go"), want: true},
		{name: "empty_target", target: "", want: false},
		{name: "dotdot_escape", target: filepath.Join("..", "workspace_sibling", "secret.txt"), want: false},
		{name: "windows_system32_traversal", target: `..\..\..\..\..\..\Windows\System32\drivers\etc\hosts`, want: false},
		{name: "sibling_prefix_collision", target: filepath.Join(outside, "file.txt"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsWithinWorkspace(ws, tc.target)
			if got != tc.want {
				t.Fatalf("IsWithinWorkspace(%q, %q) = %v, want %v", ws, tc.target, got, tc.want)
			}
		})
	}

	if runtime.GOOS == "windows" {
		// Case-insensitive drive and directory matching on Windows.
		upperTarget := strings.ToUpper(filepath.Join(ws, "src", "Main.CPP"))
		if !IsWithinWorkspace(strings.ToLower(ws), upperTarget) {
			t.Fatalf("expected case-insensitive match on Windows for %q inside %q", upperTarget, ws)
		}

		// Rooted path without drive letter (\Windows\System32) must resolve to drive root, not inside workspace.
		if IsWithinWorkspace(ws, `\Windows\System32`) {
			t.Fatalf("expected rooted path \\Windows\\System32 to be rejected")
		}

		// Directory junction escape test (mklink /J works on Windows without admin elevation).
		junctionPath := filepath.Join(ws, "escaped_junction")
		mkCmd := exec.Command("cmd.exe", "/c", "mklink", "/J", junctionPath, outside)
		if err := mkCmd.Run(); err == nil {
			escapedFile := filepath.Join(junctionPath, "nonexistent_target.txt")
			if IsWithinWorkspace(ws, escapedFile) {
				t.Fatalf("expected junction target %q -> %q to be rejected as outside workspace", escapedFile, outside)
			}
		}
	} else {
		linkPath := filepath.Join(ws, "escaped_symlink")
		if err := os.Symlink(outside, linkPath); err == nil {
			if IsWithinWorkspace(ws, filepath.Join(linkPath, "secret.txt")) {
				t.Fatalf("expected symlink escape outside workspace to be rejected")
			}
		}
	}
}

func TestGatePolicies(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	ws := filepath.Join(base, "repo")
	extDir := filepath.Join(base, "external")
	_ = os.MkdirAll(ws, 0o755)
	_ = os.MkdirAll(extDir, 0o755)

	inFile := filepath.Join(ws, "main.go")
	extFile := filepath.Join(extDir, "notes.txt")
	extSSH := filepath.Join(extDir, ".ssh", "id_rsa")
	extEnv := filepath.Join(extDir, ".env")

	t.Run("full_auto_approves_everything_without_prompt", func(t *testing.T) {
		promptCalled := false
		g := NewGate(ws, types.ApprovalFullAuto, func(ctx context.Context, req types.ApprovalRequest) (bool, error) {
			promptCalled = true
			return false, nil
		})

		reqs := []types.ApprovalRequest{
			{ToolName: "read_file", Category: types.ToolCategoryRead, TargetPath: extSSH},
			{ToolName: "edit_file", Category: types.ToolCategoryEdit, TargetPath: extFile},
			{ToolName: "run_command", Category: types.ToolCategoryExecute, Command: "echo hi"},
		}
		for _, r := range reqs {
			ok, reason, err := g.Check(ctx, r)
			if err != nil || !ok || reason != "full_auto" {
				t.Fatalf("full_auto Check(%+v) = (%v, %q, %v)", r, ok, reason, err)
			}
		}
		if promptCalled {
			t.Fatalf("PromptFunc should never be called under full_auto")
		}
	})

	t.Run("smart_auto_rules", func(t *testing.T) {
		var prompts int
		promptApprove := false
		g := NewGate(ws, types.ApprovalSmartAuto, func(ctx context.Context, req types.ApprovalRequest) (bool, error) {
			prompts++
			return promptApprove, nil
		})

		// Workspace read -> auto-approve
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "read_file", Category: types.ToolCategoryRead, TargetPath: inFile}); !ok || prompts != 0 {
			t.Fatalf("expected workspace read auto-approved without prompt")
		}
		// Empty TargetPath read (e.g. load_skill) -> auto-approve
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "load_skill", Category: types.ToolCategoryRead}); !ok || prompts != 0 {
			t.Fatalf("expected empty-target read auto-approved")
		}
		// External non-sensitive read -> auto-approve in smart_auto
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "read_file", Category: types.ToolCategoryRead, TargetPath: extFile}); !ok || prompts != 0 {
			t.Fatalf("expected non-sensitive external read auto-approved in smart_auto")
		}
		// External sensitive dotfile (.ssh/id_rsa, .env) -> must prompt!
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "read_file", Category: types.ToolCategoryRead, TargetPath: extSSH}); ok || prompts != 1 {
			t.Fatalf("expected external .ssh/id_rsa read to prompt and be denied; ok=%v prompts=%d", ok, prompts)
		}
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "read_file", Category: types.ToolCategoryRead, TargetPath: extEnv}); ok || prompts != 2 {
			t.Fatalf("expected external .env read to prompt; ok=%v prompts=%d", ok, prompts)
		}

		// Workspace edit -> auto-approve in smart_auto
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "edit_file", Category: types.ToolCategoryEdit, TargetPath: inFile}); !ok || prompts != 2 {
			t.Fatalf("expected workspace edit auto-approved in smart_auto")
		}
		// External edit -> prompts!
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "edit_file", Category: types.ToolCategoryEdit, TargetPath: extFile}); ok || prompts != 3 {
			t.Fatalf("expected external edit to prompt")
		}

		// Execute -> prompts!
		promptApprove = true
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "run_command", Category: types.ToolCategoryExecute, Command: "go test ./..."}); !ok || prompts != 4 {
			t.Fatalf("expected execute to prompt and approve when PromptFunc returns true")
		}
	})

	t.Run("strict_ask_rules", func(t *testing.T) {
		var prompts int
		g := NewGate(ws, types.ApprovalStrictAsk, func(ctx context.Context, req types.ApprovalRequest) (bool, error) {
			prompts++
			return false, nil
		})

		// Workspace read still auto-approves under ALL policies!
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "read_file", Category: types.ToolCategoryRead, TargetPath: inFile}); !ok || prompts != 0 {
			t.Fatalf("expected workspace read auto-approved even under strict_ask")
		}
		// External read prompts under strict_ask.
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "read_file", Category: types.ToolCategoryRead, TargetPath: extFile}); ok || prompts != 1 {
			t.Fatalf("expected external read to prompt under strict_ask")
		}
		// Workspace edit prompts under strict_ask.
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "write_file", Category: types.ToolCategoryEdit, TargetPath: inFile}); ok || prompts != 2 {
			t.Fatalf("expected workspace edit to prompt under strict_ask")
		}
		// Execute prompts under strict_ask.
		if ok, _, _ := g.Check(ctx, types.ApprovalRequest{ToolName: "run_command", Category: types.ToolCategoryExecute, Command: "dir"}); ok || prompts != 3 {
			t.Fatalf("expected execute to prompt under strict_ask")
		}
	})
}

func TestGateInteractiveAndNonInteractiveFallback(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	// 1. Non-interactive without custom PromptFunc must safely return (false, nil).
	nonInteractive := false
	gNon := NewGateWithOptions(GateOptions{
		WorkspaceDir: ws,
		Policy:       types.ApprovalSmartAuto,
		Interactive:  &nonInteractive,
	})
	ok, reason, err := gNon.Check(ctx, types.ApprovalRequest{
		ToolName: "run_command",
		Category: types.ToolCategoryExecute,
		Command:  "echo test",
	})
	if err != nil || ok || reason != "non_interactive_denied" {
		t.Fatalf("non-interactive Check = (%v, %q, %v), want (false, non_interactive_denied, nil)", ok, reason, err)
	}

	// 2. Interactive Stdin/Stderr reader supporting n, y, and a(lways).
	interactive := true
	input := strings.NewReader("n\ny\na\n")
	var stderr bytes.Buffer
	gInt := NewGateWithOptions(GateOptions{
		WorkspaceDir: ws,
		Policy:       types.ApprovalSmartAuto,
		Interactive:  &interactive,
		Stdin:        input,
		Stderr:       &stderr,
	})

	req := types.ApprovalRequest{
		ToolName: "run_command",
		Category: types.ToolCategoryExecute,
		Command:  "pytest -q",
	}
	if ok, _, err := gInt.Check(ctx, req); err != nil || ok {
		t.Fatalf("first answer 'n' should deny: ok=%v err=%v", ok, err)
	}
	if ok, _, err := gInt.Check(ctx, req); err != nil || !ok {
		t.Fatalf("second answer 'y' should approve: ok=%v err=%v", ok, err)
	}
	if ok, _, err := gInt.Check(ctx, req); err != nil || !ok {
		t.Fatalf("third answer 'a' should approve: ok=%v err=%v", ok, err)
	}
	if gInt.Policy() != types.ApprovalFullAuto {
		t.Fatalf("expected 'a' answer to upgrade session policy to full_auto, got %q", gInt.Policy())
	}
	// Subsequent call must not read from Stdin (which is now at EOF) because session was upgraded!
	if ok, reason, err := gInt.Check(ctx, types.ApprovalRequest{
		ToolName: "run_command",
		Category: types.ToolCategoryExecute,
		Command:  "another-cmd",
	}); err != nil || !ok || reason != "full_auto" {
		t.Fatalf("expected post-'always' command to auto-approve: (%v, %q, %v)", ok, reason, err)
	}
	if !strings.Contains(stderr.String(), "[y/N/a(lways)]") {
		t.Fatalf("expected prompt banner in stderr, got %q", stderr.String())
	}
}

func TestGateConcurrency(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	var calls atomic.Int64
	g := NewGate(ws, types.ApprovalSmartAuto, func(ctx context.Context, req types.ApprovalRequest) (bool, error) {
		calls.Add(1)
		return true, nil
	})

	policies := []types.ApprovalPolicy{
		types.ApprovalSmartAuto,
		types.ApprovalFullAuto,
		types.ApprovalStrictAsk,
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				g.SetPolicy(policies[(id+j)%len(policies)])
				_ = g.Policy()
				_, _, _ = g.Check(ctx, types.ApprovalRequest{
					ToolName:   "edit_file",
					Category:   types.ToolCategoryEdit,
					TargetPath: filepath.Join(ws, "a.go"),
				})
				_, _, _ = g.Check(ctx, types.ApprovalRequest{
					ToolName: "run_command",
					Category: types.ToolCategoryExecute,
					Command:  "go version",
				})
			}
		}(i)
	}
	wg.Wait()
}
