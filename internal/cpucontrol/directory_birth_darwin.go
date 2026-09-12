//go:build darwin

package cpucontrol

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func directoryBirthNanos(_ string, info os.FileInfo) (int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("cpucontrol: filesystem creation identity is unavailable")
	}
	return stat.Birthtimespec.Sec*1_000_000_000 + int64(stat.Birthtimespec.Nsec), nil
}

func attachDirectoryOwnership(path, token string, plain bool) error {
	if plain {
		return os.WriteFile(path+"/.payesh-owner", []byte(token), 0o600)
	}
	return unix.Setxattr(path, "com.payesh.owner", []byte(token), 0)
}

func readDirectoryOwnership(path string, plain bool) (string, error) {
	if plain {
		raw, err := os.ReadFile(path + "/.payesh-owner")
		return string(raw), err
	}
	buf := make([]byte, 128)
	n, err := unix.Getxattr(path, "com.payesh.owner", buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
