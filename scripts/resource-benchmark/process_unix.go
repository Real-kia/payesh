//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func startCommand(args []string) (*exec.Cmd, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, cmd.Start()
}

func terminateCommand(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}
