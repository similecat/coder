package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"coder/internal/permissions"
	"coder/internal/types"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return b
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.cpp", "main.cpp", true},
		{"*.cpp", "src/engine/main.cpp", true},
		{"*.cpp", "src/engine/main.h", false},
		{"**/*.cpp", "main.cpp", true},
		{"**/*.cpp", "a/b/c/main.cpp", true},
		{"src/*.cpp", "src/main.cpp", true},
		{"src/*.cpp", "src/sub/main.cpp", false},
		{"src/**/*.{h,hpp}", "src/include/vec.hpp", true},
		{"src/**/*.{h,hpp}", "src/vec.h", true},
		{"src/**/*.{h,hpp}", "src/vec.c", false},
	}
	for _, tc := range cases {
		if got := MatchGlob(tc.pattern, tc.path); got != tc.want {
			t.Fatalf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestReadFileRangesAndTruncation(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	gate := permissions.NewGate(ws, types.ApprovalSmartAuto)
	readTool := NewReadFileTool(ws, gate)

	// Create a 500-line file to test range reading and 400-line default truncation.
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		_, _ = fmt.Fprintf(&sb, "line content %03d\n", i)
	}
	filePath := filepath.Join(ws, "big.txt")
	if err := os.WriteFile(filePath, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}

	// 1. Explicit range [10, 12].
	res, err := readTool.Execute(ctx, mustJSON(t, map[string]any{
		"path":       "big.txt",
		"start_line": 10,
		"end_line":   12,
	}))
	if err != nil || res.IsError {
		t.Fatalf("read range failed: res=%+v err=%v", res, err)
	}
	wantRange := "10: line content 010\n11: line content 011\n12: line content 012"
	if res.Content != wantRange {
		t.Fatalf("unexpected range output:\ngot:  %q\nwant: %q", res.Content, wantRange)
	}

	// 2. Default truncation at 400 lines with total line count in notice.
	res, err = readTool.Execute(ctx, mustJSON(t, map[string]any{"path": "big.txt"}))
	if err != nil || res.IsError {
		t.Fatalf("default read failed: %+v %v", res, err)
	}
	if !strings.Contains(res.Content, "1: line content 001") ||
		!strings.Contains(res.Content, "400: line content 400") ||
		strings.Contains(res.Content, "401: line content 401") ||
		!strings.Contains(res.Content, "500 total lines") {
		t.Fatalf("expected truncation at 400 showing 500 total lines; got tail:\n%s", res.Content[max(0, len(res.Content)-250):])
	}

	// 3. Explicit end_line = -1 reads all 500 lines to EOF.
	res, err = readTool.Execute(ctx, mustJSON(t, map[string]any{
		"path":       "big.txt",
		"start_line": 498,
		"end_line":   -1,
	}))
	if err != nil || res.IsError {
		t.Fatalf("EOF read failed: %+v %v", res, err)
	}
	if !strings.Contains(res.Content, "500: line content 500") || strings.Contains(res.Content, "Truncated") {
		t.Fatalf("expected full EOF read without truncation, got: %q", res.Content)
	}

	// 4. 32KB byte-cap truncation on wide lines (< 400 lines but > 32KB).
	var wide strings.Builder
	chunk := strings.Repeat("x", 500)
	for i := 1; i <= 120; i++ {
		_, _ = fmt.Fprintf(&wide, "row-%03d-%s\n", i, chunk)
	}
	if err := os.WriteFile(filepath.Join(ws, "wide.txt"), []byte(wide.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = readTool.Execute(ctx, mustJSON(t, map[string]any{"path": "wide.txt"}))
	if err != nil || res.IsError {
		t.Fatalf("wide read failed: %+v %v", res, err)
	}
	if !strings.Contains(res.Content, "120 total lines") || !strings.Contains(res.Content, "Truncated:") {
		t.Fatalf("expected 32KB byte truncation showing 120 total lines, got len=%d", len(res.Content))
	}
}

func TestWriteAndEditFileWithCRLFAndAmbiguity(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	nonInteractive := false
	gate := permissions.NewGateWithOptions(permissions.GateOptions{
		WorkspaceDir: ws,
		Policy:       types.ApprovalSmartAuto,
		Interactive:  &nonInteractive,
	})

	writeTool := NewWriteFileTool(ws, gate)
	editTool := NewEditFileTool(ws, gate)

	// 1. Write inside workspace auto-approved in smart_auto; creates nested directories.
	res, err := writeTool.Execute(ctx, mustJSON(t, map[string]any{
		"path":    `sub\dir\hello.txt`,
		"content": "alpha\nbeta\ngamma\n",
	}))
	if err != nil || res.IsError {
		t.Fatalf("write_file failed: %+v %v", res, err)
	}

	// 2. Out-of-workspace write rejected in non-interactive smart_auto.
	outsidePath := filepath.Join(t.TempDir(), "escaped.txt")
	res, err = writeTool.Execute(ctx, mustJSON(t, map[string]any{
		"path":    outsidePath,
		"content": "nope",
	}))
	if err != nil || !res.IsError || !strings.Contains(res.Content, "permission denied") {
		t.Fatalf("expected outside write_file permission denial, got: %+v %v", res, err)
	}

	// 3. CRITICAL Windows CRLF test: file on disk uses \r\n, model sends \n in old_str & new_str!
	crlfPath := filepath.Join(ws, "win.cpp")
	crlfOriginal := "#include <iostream>\r\nint main() {\r\n    return 1;\r\n}\r\n"
	if err := os.WriteFile(crlfPath, []byte(crlfOriginal), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err = editTool.Execute(ctx, mustJSON(t, map[string]any{
		"path":    "win.cpp",
		"old_str": "int main() {\n    return 1;\n}",
		"new_str": "int main() {\n    std::cout << \"ok\";\n    return 0;\n}",
	}))
	if err != nil || res.IsError {
		t.Fatalf("CRLF edit_file failed: %+v %v", res, err)
	}

	afterBytes, err := os.ReadFile(crlfPath)
	if err != nil {
		t.Fatal(err)
	}
	after := string(afterBytes)
	wantCRLF := "#include <iostream>\r\nint main() {\r\n    std::cout << \"ok\";\r\n    return 0;\r\n}\r\n"
	if after != wantCRLF {
		t.Fatalf("CRLF preservation mismatch!\ngot:  %q\nwant: %q", after, wantCRLF)
	}

	// 4. Duplicate old_str ambiguity error vs replace_all=true.
	dupPath := filepath.Join(ws, "dup.py")
	if err := os.WriteFile(dupPath, []byte("x = 1\ny = 2\nx = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = editTool.Execute(ctx, mustJSON(t, map[string]any{
		"path":    "dup.py",
		"old_str": "x = 1",
		"new_str": "x = 99",
	}))
	if err != nil || !res.IsError || !strings.Contains(res.Content, "matched 2 times") || !strings.Contains(res.Content, "1, 3") {
		t.Fatalf("expected actionable duplicate diagnostic with line numbers 1, 3; got: %+v", res)
	}

	res, err = editTool.Execute(ctx, mustJSON(t, map[string]any{
		"path":        "dup.py",
		"old_str":     "x = 1",
		"new_str":     "x = 99",
		"replace_all": true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("replace_all edit failed: %+v %v", res, err)
	}
	dupAfter, _ := os.ReadFile(dupPath)
	if string(dupAfter) != "x = 99\ny = 2\nx = 99\n" {
		t.Fatalf("unexpected replace_all content: %q", string(dupAfter))
	}
}

func TestListDirAndGrepSearch(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	mustWrite := func(rel, body string) {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite(".gitignore", "ignored_out/\n")
	mustWrite("src/algo.cpp", "int solveQuest(int n) {\n    return n * 2;\n}\n")
	mustWrite("src/helper.py", "def solve_quest(x):\n    return x + 1\n")
	mustWrite("node_modules/pkg/index.js", "function solveQuest() {}\n")
	mustWrite(".git/config", "solveQuest\n")
	mustWrite("bin/artifact.txt", "solveQuest\n")
	mustWrite("ignored_out/gen.cpp", "int solveQuest() { return 0; }\n")
	// Binary file containing NUL bytes + search keyword must be skipped!
	if err := os.WriteFile(filepath.Join(ws, "src", "blob.bin"), []byte("solveQuest\x00\x01\x02binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. list_dir skips ignored directories & .gitignore dirs unless explicitly targeted.
	listTool := NewListDirTool(ws, nil)
	res, err := listTool.Execute(ctx, mustJSON(t, map[string]any{}))
	if err != nil || res.IsError {
		t.Fatalf("list_dir failed: %+v %v", res, err)
	}
	if strings.Contains(res.Content, "node_modules") ||
		strings.Contains(res.Content, ".git/") ||
		strings.Contains(res.Content, ".git/config") ||
		strings.Contains(res.Content, "bin/") ||
		strings.Contains(res.Content, "ignored_out") ||
		!strings.Contains(res.Content, "src/algo.cpp") {
		t.Fatalf("unexpected list_dir filtering:\n%s", res.Content)
	}

	// Explicitly targeting 'bin' lists its contents.
	resBin, err := listTool.Execute(ctx, mustJSON(t, map[string]any{"path": "bin"}))
	if err != nil || resBin.IsError || !strings.Contains(resBin.Content, "artifact.txt") {
		t.Fatalf("expected explicit bin target to list artifact.txt, got: %+v %v", resBin, err)
	}

	// Glob filter '**/*.cpp'
	resGlob, err := listTool.Execute(ctx, mustJSON(t, map[string]any{"pattern": "**/*.cpp"}))
	if err != nil || resGlob.IsError || strings.TrimSpace(resGlob.Content) != "src/algo.cpp" {
		t.Fatalf("expected glob '**/*.cpp' -> 'src/algo.cpp', got: %q", resGlob.Content)
	}

	// 2. Test pure-Go concurrent search directly!
	pureOut, count, truncated, err := GrepPureGo(ctx, GrepOptions{
		WorkspaceDir:    ws,
		TargetPath:      ws,
		Pattern:         `solve_?quest`,
		CaseInsensitive: true,
		ContextLines:    1,
		MaxResults:      10,
	})
	if err != nil {
		t.Fatalf("GrepPureGo error: %v", err)
	}
	if truncated || count != 2 {
		t.Fatalf("expected 2 pure-Go matches (algo.cpp & helper.py, skipping binary & ignored dirs), got count=%d:\n%s", count, pureOut)
	}
	if !strings.Contains(pureOut, "src/algo.cpp:1: int solveQuest(int n) {") ||
		!strings.Contains(pureOut, "src/helper.py:1: def solve_quest(x):") ||
		strings.Contains(pureOut, "blob.bin") ||
		strings.Contains(pureOut, "node_modules") {
		t.Fatalf("unexpected GrepPureGo output:\n%s", pureOut)
	}

	// 3. Test grep_search tool end-to-end (both default/rg and forced pure-Go).
	for _, disableRg := range []bool{false, true} {
		grepTool := newGrepSearchToolWithOptions(ws, nil, disableRg)
		gRes, err := grepTool.Execute(ctx, mustJSON(t, map[string]any{
			"pattern": "solveQuest",
			"glob":    "*.cpp",
		}))
		if err != nil || gRes.IsError {
			t.Fatalf("grep_search (disableRg=%v) failed: %+v %v", disableRg, gRes, err)
		}
		if !strings.Contains(gRes.Content, "src/algo.cpp:1: int solveQuest(int n) {") {
			t.Fatalf("grep_search (disableRg=%v) unexpected output: %q", disableRg, gRes.Content)
		}
	}
}

func TestRunCommandExitTimeoutTruncationAndMSVC(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	gate := permissions.NewGate(ws, types.ApprovalFullAuto)

	// 1. Success & non-zero exit code.
	runTool := newRunCommandToolWithCap(ws, gate, 600)
	okRes, err := runTool.Execute(ctx, mustJSON(t, map[string]any{
		"command": "Write-Output 'hello-runner'",
	}))
	if err != nil || okRes.IsError || !strings.Contains(okRes.Content, "hello-runner") {
		t.Fatalf("expected successful run_command, got: %+v %v", okRes, err)
	}

	failCommand := "Write-Output 'bad-state'; exit 7"
	if runtime.GOOS != "windows" {
		failCommand = "echo bad-state; exit 7"
	}
	failRes, err := runTool.Execute(ctx, mustJSON(t, map[string]any{
		"command": failCommand,
	}))
	if err != nil || !failRes.IsError || !strings.Contains(failRes.Content, "bad-state") || !strings.Contains(failRes.Content, "exit_code=7") {
		t.Fatalf("expected exit_code=7 IsError=true, got: %+v %v", failRes, err)
	}

	// 2. Head + tail output truncation on large output (> 600 byte cap).
	spamCmd := "Write-Output 'TOP_ERROR_LINE'; 1..120 | ForEach-Object { Write-Output ('filler_line_' + $_) }; Write-Output 'BOTTOM_SUMMARY_LINE'"
	if runtime.GOOS != "windows" {
		spamCmd = "echo TOP_ERROR_LINE; seq 1 120 | sed 's/^/filler_line_/'; echo BOTTOM_SUMMARY_LINE"
	}
	truncRes, err := runTool.Execute(ctx, mustJSON(t, map[string]any{
		"command": spamCmd,
	}))
	if err != nil || truncRes.IsError {
		t.Fatalf("spam command failed: %+v %v", truncRes, err)
	}
	if !strings.Contains(truncRes.Content, "TOP_ERROR_LINE") ||
		!strings.Contains(truncRes.Content, "BOTTOM_SUMMARY_LINE") ||
		!strings.Contains(truncRes.Content, "bytes omitted from middle") {
		t.Fatalf("expected head + omitted marker + tail in truncated output, got:\n%s", truncRes.Content)
	}

	// 3. Timeout cancellation.
	sleepCmd := "Start-Sleep -Seconds 30"
	if runtime.GOOS != "windows" {
		sleepCmd = "sleep 30"
	}
	t0 := time.Now()
	timeoutRes, err := runTool.Execute(ctx, mustJSON(t, map[string]any{
		"command":     sleepCmd,
		"timeout_sec": 1,
	}))
	elapsed := time.Since(t0)
	if err != nil || !timeoutRes.IsError || !strings.Contains(timeoutRes.Content, "timed out") {
		t.Fatalf("expected timeout error result, got: %+v %v", timeoutRes, err)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("timeout took too long to kill process tree: %v", elapsed)
	}

	// 4. Windows MSVC C++ cl.exe Accelerator verification!
	if runtime.GOOS == "windows" {
		vcvars := FindVCVars64()
		if vcvars == "" {
			t.Log("vcvars64.bat not installed on this machine; skipping cl.exe compile check")
			return
		}
		t.Logf("Found vcvars64.bat at: %s", vcvars)

		cppTool := NewRunCommandTool(ws, gate)
		cppSrc := "#include <iostream>\r\n#include <vector>\r\nint main() {\r\n    std::vector<int> v = {10, 20, 12};\r\n    std::cout << \"MSVC_CPP_OK:\" << (v[0] + v[1] + v[2]) << std::endl;\r\n    return 0;\r\n}\r\n"
		if err := os.WriteFile(filepath.Join(ws, "hello_msvc.cpp"), []byte(cppSrc), 0o644); err != nil {
			t.Fatal(err)
		}

		cppRes, err := cppTool.Execute(ctx, mustJSON(t, map[string]any{
			"command": `cl.exe /nologo /EHsc /std:c++17 hello_msvc.cpp; if ($LASTEXITCODE -eq 0) { .\hello_msvc.exe }`,
		}))
		if err != nil || cppRes.IsError || !strings.Contains(cppRes.Content, "MSVC_CPP_OK:42") {
			t.Fatalf("MSVC cl.exe compile+run failed!\nResult: %+v\nErr: %v", cppRes, err)
		}

		// Verify ResolveMSVCEnv() is cached in memory (0ms overhead on subsequent calls).
		cachedStart := time.Now()
		envSlice := ResolveMSVCEnv()
		if cachedDur := time.Since(cachedStart); cachedDur > 25*time.Millisecond || len(envSlice) == 0 {
			t.Fatalf("expected cached ResolveMSVCEnv() in <25ms, took %v (len=%d)", cachedDur, len(envSlice))
		}
	}
}

type mockSkillCatalog struct {
	skills  []types.SkillInfo
	content map[string]string
	helpers map[string][]string
}

func (m *mockSkillCatalog) List() []types.SkillInfo { return m.skills }

func (m *mockSkillCatalog) Load(name string) (string, []string, types.SkillInfo, error) {
	for _, s := range m.skills {
		if s.Name == name {
			return m.content[name], m.helpers[name], s, nil
		}
	}
	return "", nil, types.SkillInfo{}, fmt.Errorf("skill %q not found", name)
}

func TestLoadSkillCustomToolAndToolset(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "app.py"), []byte("print('hi')\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(ws, "node_modules", "ignored"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "node_modules", "ignored", "x.js"), []byte("x"), 0o644)

	// 1. Without skills, load_skill is omitted from Toolset.
	tsEmpty := NewToolset(ToolsetOptions{WorkspaceDir: ws})
	if _, ok := tsEmpty.Get("load_skill"); ok {
		t.Fatalf("load_skill should not be registered when SkillCatalog is nil")
	}

	// 2. With non-empty SkillCatalog + CustomTool, both are registered and executable.
	cat := &mockSkillCatalog{
		skills: []types.SkillInfo{
			{
				Name:        "cpp-opt",
				Description: "C++ optimization guide",
				SkillFile:   filepath.Join(ws, ".coder", "skills", "cpp-opt", "SKILL.md"),
				Dir:         filepath.Join(ws, ".coder", "skills", "cpp-opt"),
				Source:      types.SourceCoderRepo,
			},
		},
		content: map[string]string{
			"cpp-opt": "# C++ Optimization\nUse reserve() on std::vector.",
		},
		helpers: map[string][]string{
			"cpp-opt": {"scripts/bench.py", "templates/fast_io.hpp"},
		},
	}

	// Build a small portable Go helper binary in ws to test CustomTool argv safety, stdin, and CODER_TOOL_ARGS!
	echoScript := filepath.Join(ws, "inspect_custom.py")
	// Use powershell.exe with -Command reading env + args, OR go env / cmd, let's check if powershell.exe argv works cleanly:
	// With powershell.exe -NoLogo -NoProfile -File <script.ps1>, argv elements are passed directly in $args without shell parsing!
	psScript := filepath.Join(ws, "echo_tool.ps1")
	psBody := `$stdinText = [Console]::In.ReadToEnd()
Write-Output ("ARG0=" + $args[0])
Write-Output ("ENV=" + $env:CODER_TOOL_ARGS)
Write-Output ("STDIN=" + $stdinText)
`
	if err := os.WriteFile(psScript, []byte(psBody), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = echoScript

	var customSpec types.CustomToolSpec
	if runtime.GOOS == "windows" {
		customSpec = types.CustomToolSpec{
			Name:        "custom_echo",
			Description: "Echo custom tool args safely",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"payload": map[string]any{"type": "string"},
				},
			},
			Command:  "powershell.exe",
			Args:     []string{"-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", psScript, "{{payload}}"},
			Shell:    false,
			ReadOnly: true,
		}
	} else {
		customSpec = types.CustomToolSpec{
			Name:        "custom_echo",
			Description: "Echo custom tool args safely",
			Command:     "/bin/sh",
			Args:        []string{"-c", `printf "ARG0=%s\nENV=%s\nSTDIN=%s\n" "$1" "$CODER_TOOL_ARGS" "$(cat)"`, "sh", "{{payload}}"},
			Shell:       false,
			ReadOnly:    true,
		}
	}

	nonInteractive := false
	gate := permissions.NewGateWithOptions(permissions.GateOptions{
		WorkspaceDir: ws,
		Policy:       types.ApprovalSmartAuto,
		Interactive:  &nonInteractive,
	})

	// Also add a non-readonly custom tool to verify permission gating blocks it in non-interactive smart_auto!
	restrictedSpec := types.CustomToolSpec{
		Name:        "deploy_prod",
		Description: "Dangerous deploy command",
		Command:     "powershell.exe",
		Args:        []string{"-Command", "Write-Output 'deployed'"},
		ReadOnly:    false,
	}

	ts := NewToolset(ToolsetOptions{
		WorkspaceDir: ws,
		Approver:     gate,
		Skills:       cat,
		CustomTools:  []types.CustomToolSpec{customSpec, restrictedSpec},
	})

	if len(ts.Definitions()) != 9 {
		t.Fatalf("expected 6 built-ins + load_skill + 2 custom tools = 9 definitions, got %d", len(ts.Definitions()))
	}

	// Execute load_skill
	skillRes, err := ts.Execute(ctx, "load_skill", mustJSON(t, map[string]any{"skill_name": "cpp-opt"}))
	if err != nil || skillRes.IsError {
		t.Fatalf("load_skill failed: %+v %v", skillRes, err)
	}
	if !strings.Contains(skillRes.Content, "Use reserve() on std::vector.") ||
		!strings.Contains(skillRes.Content, "scripts/bench.py") ||
		!strings.Contains(skillRes.Content, filepath.Join(ws, ".coder", "skills", "cpp-opt")) {
		t.Fatalf("unexpected load_skill output:\n%s", skillRes.Content)
	}

	// Execute custom_echo with shell-metacharacter payload to verify zero shell injection in argv mode!
	trickyPayload := "safe_val; Write-Output SHELL_INJECTED"
	customRes, err := ts.Execute(ctx, "custom_echo", mustJSON(t, map[string]any{"payload": trickyPayload}))
	if err != nil || customRes.IsError {
		t.Fatalf("custom_echo failed: %+v %v", customRes, err)
	}
	if !strings.Contains(customRes.Content, "ARG0="+trickyPayload) ||
		!strings.Contains(customRes.Content, "ENV={\"payload\":") ||
		!strings.Contains(customRes.Content, "STDIN={\"payload\":") {
		t.Fatalf("unexpected custom_echo output:\n%s", customRes.Content)
	}

	// Execute non-readonly custom tool under non-interactive smart_auto -> must be denied by Gate!
	deniedRes, err := ts.Execute(ctx, "deploy_prod", mustJSON(t, map[string]any{}))
	if err != nil || !deniedRes.IsError || !strings.Contains(deniedRes.Content, "permission denied") {
		t.Fatalf("expected deploy_prod to be denied by smart_auto gate, got: %+v %v", deniedRes, err)
	}

	// WorkspaceFiles check
	wsFiles := ts.WorkspaceFiles(100)
	foundApp := false
	for _, f := range wsFiles {
		if f == "app.py" {
			foundApp = true
		}
		if strings.Contains(f, "node_modules") {
			t.Fatalf("WorkspaceFiles should skip node_modules, got %q", f)
		}
	}
	if !foundApp {
		t.Fatalf("expected app.py in WorkspaceFiles, got %v", wsFiles)
	}
}
