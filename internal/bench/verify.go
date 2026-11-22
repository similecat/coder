package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"coder/internal/tools"
)

const (
	pythonVerifyTimeout = 60 * time.Second
	cppCompileTimeout   = 60 * time.Second
	cppRunTimeout       = 30 * time.Second
	maxVerifyOutputCap  = 64 * 1024
)

// VerifyResult records the outcome of tamper-proof host verification against a workspace.
type VerifyResult struct {
	Passed     bool          `json:"passed"`
	ExitCode   int           `json:"exit_code"`
	Compiler   string        `json:"compiler,omitempty"`
	Output     string        `json:"output,omitempty"`
	Duration   time.Duration `json:"-"`
	DurationMs int64         `json:"duration_ms"`
	Error      string        `json:"error,omitempty"`
}

// VerifyWorkspace runs the problem's untouched host verification suite against workspaceDir
// from an isolated temporary build/execution directory so the agent can never tamper with verify/.
func VerifyWorkspace(ctx context.Context, prob Problem, workspaceDir string) VerifyResult {
	start := time.Now()
	finish := func(res VerifyResult) VerifyResult {
		res.Duration = time.Since(start)
		res.DurationMs = res.Duration.Milliseconds()
		res.Output = capVerifyOutput(strings.TrimSpace(res.Output))
		return res
	}

	absWS, err := filepath.Abs(filepath.Clean(workspaceDir))
	if err != nil {
		return finish(VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    fmt.Sprintf("resolve workspace path: %v", err),
		})
	}
	if fi, statErr := os.Stat(absWS); statErr != nil || !fi.IsDir() {
		return finish(VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    fmt.Sprintf("workspace directory does not exist: %s", absWS),
		})
	}

	verifyEntryPath := filepath.Join(prob.Dir, filepath.FromSlash(prob.VerifyEntry))
	if fi, statErr := os.Stat(verifyEntryPath); statErr != nil || fi.IsDir() {
		return finish(VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    fmt.Sprintf("host verify entry missing: %s", verifyEntryPath),
		})
	}

	switch prob.Language {
	case "python":
		return finish(verifyPythonWorkspace(ctx, prob, verifyEntryPath, absWS))
	case "cpp":
		return finish(verifyCPPWorkspace(ctx, prob, verifyEntryPath, absWS))
	default:
		return finish(VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    fmt.Sprintf("unsupported problem language %q", prob.Language),
		})
	}
}

func verifyPythonWorkspace(ctx context.Context, prob Problem, verifyScriptPath, absWS string) VerifyResult {
	pyExe, err := findPythonExecutable()
	if err != nil {
		return VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    err.Error(),
		}
	}

	verifyTmpDir, err := os.MkdirTemp("", "coder-verify-py-"+prob.ID+"-")
	if err != nil {
		return VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    fmt.Sprintf("create temp verify dir: %v", err),
		}
	}
	defer func() { _ = os.RemoveAll(verifyTmpDir) }()

	runCtx, cancel := context.WithTimeout(ctx, pythonVerifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, pyExe, "-B", verifyScriptPath)
	cmd.Dir = verifyTmpDir
	cmd.Env = overlayEnv(os.Environ(), map[string]string{
		"WORKSPACE_DIR":           absWS,
		"PYTHONPATH":              absWS,
		"PYTHONDONTWRITEBYTECODE": "1",
	})
	cmd.WaitDelay = 3 * time.Second

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	exitCode := extractProcessExitCode(runErr, timedOut)

	res := VerifyResult{
		Passed:   runErr == nil && exitCode == 0 && !timedOut,
		ExitCode: exitCode,
		Compiler: filepath.Base(pyExe),
		Output:   buf.String(),
	}
	if timedOut {
		res.Error = fmt.Sprintf("python verification timed out after %s", pythonVerifyTimeout)
	} else if runErr != nil {
		res.Error = fmt.Sprintf("python verification exited with code %d", exitCode)
	}
	return res
}

func verifyCPPWorkspace(ctx context.Context, prob Problem, verifyMainPath, absWS string) VerifyResult {
	verifyTmpDir, err := os.MkdirTemp("", "coder-verify-cpp-"+prob.ID+"-")
	if err != nil {
		return VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    fmt.Sprintf("create temp verify build dir: %v", err),
		}
	}
	defer func() { _ = os.RemoveAll(verifyTmpDir) }()

	cppSourcePaths := make([]string, 0, len(prob.CPPSources))
	for _, src := range prob.CPPSources {
		srcPath := filepath.Join(absWS, filepath.FromSlash(src))
		if fi, statErr := os.Stat(srcPath); statErr != nil || fi.IsDir() {
			return VerifyResult{
				Passed:   false,
				ExitCode: -1,
				Error:    fmt.Sprintf("required C++ workspace source missing: %s", src),
			}
		}
		cppSourcePaths = append(cppSourcePaths, srcPath)
	}

	binName := "verify_bin"
	if runtime.GOOS == "windows" {
		binName = "verify_bin.exe"
	}
	outExe := filepath.Join(verifyTmpDir, binName)

	compilerLabel, compilerExe, compileArgs, compileEnv, err := resolveCPPCompileSpec(
		absWS,
		verifyMainPath,
		cppSourcePaths,
		outExe,
	)
	if err != nil {
		return VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Error:    err.Error(),
		}
	}

	compileCtx, cancelCompile := context.WithTimeout(ctx, cppCompileTimeout)
	defer cancelCompile()

	compileCmd := exec.CommandContext(compileCtx, compilerExe, compileArgs...)
	compileCmd.Dir = verifyTmpDir
	compileCmd.Env = compileEnv
	compileCmd.WaitDelay = 3 * time.Second

	var compileBuf bytes.Buffer
	compileCmd.Stdout = &compileBuf
	compileCmd.Stderr = &compileBuf

	compileErr := compileCmd.Run()
	compileTimedOut := errors.Is(compileCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	if compileErr != nil || compileTimedOut {
		code := extractProcessExitCode(compileErr, compileTimedOut)
		errMsg := fmt.Sprintf("C++ compilation failed (%s, exit code %d)", compilerLabel, code)
		if compileTimedOut {
			errMsg = fmt.Sprintf("C++ compilation timed out after %s (%s)", cppCompileTimeout, compilerLabel)
		}
		return VerifyResult{
			Passed:   false,
			ExitCode: code,
			Compiler: compilerLabel,
			Output:   compileBuf.String(),
			Error:    errMsg,
		}
	}

	if fi, statErr := os.Stat(outExe); statErr != nil || fi.IsDir() {
		return VerifyResult{
			Passed:   false,
			ExitCode: -1,
			Compiler: compilerLabel,
			Output:   compileBuf.String(),
			Error:    "compiler succeeded without producing verify binary",
		}
	}

	runCtx, cancelRun := context.WithTimeout(ctx, cppRunTimeout)
	defer cancelRun()

	runCmd := exec.CommandContext(runCtx, outExe)
	runCmd.Dir = verifyTmpDir
	runCmd.Env = overlayEnv(compileEnv, map[string]string{
		"WORKSPACE_DIR": absWS,
	})
	runCmd.WaitDelay = 3 * time.Second

	var runBuf bytes.Buffer
	runCmd.Stdout = &runBuf
	runCmd.Stderr = &runBuf

	runErr := runCmd.Run()
	runTimedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	exitCode := extractProcessExitCode(runErr, runTimedOut)

	combinedOut := strings.TrimSpace(runBuf.String())
	if compWarn := strings.TrimSpace(compileBuf.String()); compWarn != "" && combinedOut == "" {
		combinedOut = compWarn
	}

	res := VerifyResult{
		Passed:   runErr == nil && exitCode == 0 && !runTimedOut,
		ExitCode: exitCode,
		Compiler: compilerLabel,
		Output:   combinedOut,
	}
	if runTimedOut {
		res.Error = fmt.Sprintf("verification binary timed out after %s", cppRunTimeout)
	} else if runErr != nil {
		res.Error = fmt.Sprintf("verification binary failed with exit code %d", exitCode)
	}
	return res
}

func resolveCPPCompileSpec(
	absWS string,
	verifyMainPath string,
	cppSourcePaths []string,
	outExe string,
) (label string, exe string, args []string, env []string, err error) {
	if runtime.GOOS == "windows" {
		msvcEnv := tools.ResolveMSVCEnv()
		if clExe, ok := findExecutableInEnv("cl.exe", msvcEnv); ok {
			clArgs := make([]string, 0, 8+len(cppSourcePaths))
			clArgs = append(clArgs,
				"/nologo",
				"/EHsc",
				"/std:c++20",
				"/O2",
				"/W4",
				"/I"+absWS,
				verifyMainPath,
			)
			clArgs = append(clArgs, cppSourcePaths...)
			clArgs = append(clArgs, "/Fe:"+outExe)
			return "cl.exe", clExe, clArgs, msvcEnv, nil
		}

		for _, gnuCompiler := range []string{"g++", "clang++"} {
			if gnuExe, ok := findExecutableInEnv(gnuCompiler, msvcEnv); ok {
				gnuArgs := buildGNUCPPArgs(absWS, verifyMainPath, cppSourcePaths, outExe, false)
				return gnuCompiler, gnuExe, gnuArgs, msvcEnv, nil
			}
		}
		return "", "", nil, nil, errors.New("no C++20 compiler found on Windows (checked MSVC cl.exe via vcvars64, g++, and clang++)")
	}

	baseEnv := os.Environ()
	for _, gnuCompiler := range []string{"g++", "clang++"} {
		if gnuExe, lookErr := exec.LookPath(gnuCompiler); lookErr == nil {
			gnuArgs := buildGNUCPPArgs(absWS, verifyMainPath, cppSourcePaths, outExe, true)
			return gnuCompiler, gnuExe, gnuArgs, baseEnv, nil
		}
	}
	return "", "", nil, nil, errors.New("no C++20 compiler found (checked g++ and clang++)")
}

func buildGNUCPPArgs(absWS, verifyMainPath string, cppSourcePaths []string, outExe string, extraWarnings bool) []string {
	args := make([]string, 0, 9+len(cppSourcePaths))
	args = append(args, "-std=c++20", "-O2")
	if extraWarnings {
		args = append(args, "-Wall", "-Wextra")
	}
	args = append(args, "-I"+absWS, verifyMainPath)
	args = append(args, cppSourcePaths...)
	args = append(args, "-o", outExe)
	return args
}

func findPythonExecutable() (string, error) {
	for _, name := range []string{"python", "python3"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("python interpreter not found in PATH (checked python and python3)")
}

func findExecutableInEnv(name string, env []string) (string, bool) {
	var pathValue string
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if strings.EqualFold(kv[:eq], "PATH") {
			pathValue = kv[eq+1:]
			break
		}
	}

	candidates := []string{name}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		candidates = []string{name + ".exe", name + ".cmd", name + ".bat", name}
	}

	if pathValue != "" {
		for _, dir := range filepath.SplitList(pathValue) {
			dir = strings.Trim(strings.TrimSpace(dir), `"`)
			if dir == "" {
				continue
			}
			for _, cand := range candidates {
				full := filepath.Join(dir, cand)
				if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
					return full, true
				}
			}
		}
	}

	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	return "", false
}

func overlayEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}

	type kvPair struct {
		key string
		val string
	}
	ordered := make([]kvPair, 0, len(base)+len(overrides))
	idxByKey := make(map[string]int, len(base)+len(overrides))

	norm := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}

	put := func(k, v string) {
		if k == "" || strings.HasPrefix(k, "=") {
			return
		}
		nk := norm(k)
		if i, ok := idxByKey[nk]; ok {
			ordered[i].val = v
			return
		}
		idxByKey[nk] = len(ordered)
		ordered = append(ordered, kvPair{key: k, val: v})
	}

	for _, raw := range base {
		eq := strings.IndexByte(raw, '=')
		if eq <= 0 {
			continue
		}
		put(raw[:eq], raw[eq+1:])
	}
	for k, v := range overrides {
		put(k, v)
	}

	out := make([]string, len(ordered))
	for i, item := range ordered {
		out[i] = item.key + "=" + item.val
	}
	return out
}

func extractProcessExitCode(runErr error, timedOut bool) int {
	if runErr == nil && !timedOut {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if code := exitErr.ExitCode(); code != 0 {
			return code
		}
	}
	if timedOut {
		return -1
	}
	return 1
}

func capVerifyOutput(s string) string {
	if len(s) <= maxVerifyOutputCap {
		return s
	}
	half := (maxVerifyOutputCap - 64) / 2
	return fmt.Sprintf("%s\n...[truncated %d bytes]...\n%s", s[:half], len(s)-2*half, s[len(s)-half:])
}
