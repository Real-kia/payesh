//go:build darwin

package cpucontrol

import (
	"errors"
	"os"
	"syscall"
)

func directoryBirthNanos(_ string, info os.FileInfo) (int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("cpucontrol: filesystem creation identity is unavailable")
	}
	return stat.Birthtimespec.Sec*1_000_000_000 + int64(stat.Birthtimespec.Nsec), nil
}
