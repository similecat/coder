//go:build !windows

package tools

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func buildShellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	configureChildProcess(cmd)
	return cmd
}

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 2 * time.Second
}

func captureVCVarsEnvironment(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("vcvars64 is only supported on Windows")
}
