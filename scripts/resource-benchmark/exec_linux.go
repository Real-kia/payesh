//go:build linux

package main

import "os/exec"

func execGetconf() (string, error) {
	b, err := exec.Command("getconf", "CLK_TCK").Output()
	return string(b), err
}
