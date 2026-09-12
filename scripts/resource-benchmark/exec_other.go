//go:build !linux

package main

func execGetconf() (string, error) { return "100", nil }
