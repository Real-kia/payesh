//go:build linux

package cpucontrol

import (
	"os"

	"golang.org/x/sys/unix"
)

const directoryOwnershipXattr = "trusted.payesh.owner"

func directoryBirthNanos(path string, _ os.FileInfo) (int64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_BTIME == 0 {
		// cgroup2 commonly omits STATX_BTIME. The cryptographically random
		// trusted xattr remains the non-reusable identity in that case.
		return 0, nil
	}
	return stat.Btime.Sec*1_000_000_000 + int64(stat.Btime.Nsec), nil
}

func attachDirectoryOwnership(path, token string, plain bool) error {
	if plain {
		return os.WriteFile(path+"/.payesh-owner", []byte(token), 0o600)
	}
	return unix.Setxattr(path, directoryOwnershipXattr, []byte(token), 0)
}

func readDirectoryOwnership(path string, plain bool) (string, error) {
	if plain {
		raw, err := os.ReadFile(path + "/.payesh-owner")
		return string(raw), err
	}
	buf := make([]byte, 128)
	n, err := unix.Getxattr(path, directoryOwnershipXattr, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
