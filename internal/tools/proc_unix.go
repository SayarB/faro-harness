//go:build !windows

package tools

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

func shellDisplayName() string {
	if _, err := exec.LookPath("bash"); err == nil {
		return runtime.GOOS + " bash"
	}
	return runtime.GOOS + " sh"
}

func buildShellCommand(ctx context.Context, cmdStr string) *exec.Cmd {
	shellPath, err := exec.LookPath("bash")
	if err != nil {
		if sh, shErr := exec.LookPath("sh"); shErr == nil {
			shellPath = sh
		} else {
			shellPath = "/bin/sh"
		}
	}
	cmd := exec.CommandContext(ctx, shellPath, "-c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pgid := cmd.Process.Pid
		// Send SIGTERM to the whole process group first so scripts can flush/clean up,
		// then escalate to SIGKILL if any grandchild is still alive.
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		deadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) {
			if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return cmd
}

func extractSignal(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}

// cleanupProcGroup unconditionally kills any remaining processes in cmd's process group
// after cmd exits (preventing background children like `sleep 77 &` from outliving the tool).
func cleanupProcGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
