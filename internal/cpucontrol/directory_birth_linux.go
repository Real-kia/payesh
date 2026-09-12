//go:build linux

package cpucontrol

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func directoryBirthNanos(path string, _ os.FileInfo) (int64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_BTIME == 0 {
		return 0, errors.New("cpucontrol: filesystem creation identity is unavailable")
	}
	return stat.Btime.Sec*1_000_000_000 + int64(stat.Btime.Nsec), nil
}
