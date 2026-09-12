//go:build windows

package main

import "os/exec"

func startCommand(args []string) (*exec.Cmd, error) {
	cmd := exec.Command(args[0], args[1:]...)
	return cmd, cmd.Start()
}
func terminateCommand(pid int) {
	_ = exec.Command("taskkill", "/PID", fmt.Sprint(pid), "/T", "/F").Run()
}
