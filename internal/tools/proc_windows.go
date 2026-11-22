//go:build windows

package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func buildShellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx,
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", command,
	)
	configureChildProcess(cmd)
	return cmd
}

func configureChildProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Terminate the full Windows process tree so child compilers/test binaries close pipes immediately.
		killCmd := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		_ = killCmd.Run()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 2 * time.Second
}

func captureVCVarsEnvironment(ctx context.Context, vcvarsPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	// Use SysProcAttr.CmdLine so cmd.exe /s /c receives unescaped inner quotes around vcvars64.bat paths containing spaces.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: fmt.Sprintf(`cmd.exe /d /s /c "call "%s" >nul 2>&1 && set"`, vcvarsPath),
	}
	configureChildProcess(cmd)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}
