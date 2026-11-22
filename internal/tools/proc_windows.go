//go:build windows

package tools

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func shellDisplayName() string {
	return "Windows cmd.exe"
}

func buildShellCommand(ctx context.Context, cmdStr string) *exec.Cmd {
	// Pass raw CmdLine so cmd.exe receives quotes verbatim instead of CommandLineToArgv escaping.
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `cmd.exe /S /C "` + cmdStr + `"`,
	}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Terminate the full process tree so grandchildren holding pipes do not hang Wait().
		killCmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := killCmd.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return cmd
}

func extractSignal(_ error) string {
	return ""
}

// cleanupProcGroup ensures any lingering background children spawned by cmd are terminated.
func cleanupProcGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
