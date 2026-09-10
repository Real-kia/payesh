package cpucontrol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcessIdentity pins a policy to one specific process instance, not just a
// PID number. PID values are reused by the kernel; without this, a policy
// applied to PID 4242 could silently keep applying to an unrelated later
// process that happens to reuse 4242 after the original exits.
//
// StartTicks is /proc/<pid>/stat field 22 (starttime, in clock ticks since
// boot) — the portable, no-special-privilege signal this package checks.
// Linux also offers pidfd_open, which pins identity via a stable file
// descriptor instead of a re-read comparison; that is a real-Linux-only
// enhancement left for the next integration pass (see docs/handoffs/08.md)
// rather than something a plain /proc read can exercise or this development
// host can validate.
type ProcessIdentity struct {
	PID        int
	StartTicks uint64
}

// ReadProcessIdentity reads PID's current identity from procRoot (in
// production, "/proc"; tests supply a fixture directory with the same
// <pid>/stat layout).
func ReadProcessIdentity(procRoot string, pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, errors.New("cpucontrol: pid must be positive")
	}
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcessIdentity{}, err
	}
	ticks, err := parseStartTicks(string(raw))
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{PID: pid, StartTicks: ticks}, nil
}

// parseStartTicks extracts field 22 (starttime) from a /proc/<pid>/stat
// line. The second field is the process comm name in parentheses and may
// itself contain spaces or parentheses, so field counting starts after the
// last ')' rather than by naively splitting on whitespace from the front.
func parseStartTicks(line string) (uint64, error) {
	closeParen := strings.LastIndexByte(line, ')')
	if closeParen < 0 || closeParen+2 > len(line) {
		return 0, fmt.Errorf("cpucontrol: stat line has an unexpected format")
	}
	rest := strings.Fields(line[closeParen+2:])
	// rest[0] is field 3 (state), the first space-delimited field after the
	// "(comm) " prefix. starttime is overall field 22, so it is rest[22-3]
	// = rest[19].
	const startTimeIndex = 19
	if len(rest) <= startTimeIndex {
		return 0, fmt.Errorf("cpucontrol: stat line is too short to contain starttime")
	}
	ticks, err := strconv.ParseUint(rest[startTimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cpucontrol: starttime field is invalid: %w", err)
	}
	return ticks, nil
}

// VerifyIdentity re-reads the process's current identity and reports whether
// it still matches expected. A process that has exited (stat file gone) or
// whose PID has been reused by a different process (start ticks differ)
// both report false, never an error that could be mistaken for "verified".
func VerifyIdentity(procRoot string, expected ProcessIdentity) (bool, error) {
	current, err := ReadProcessIdentity(procRoot, expected.PID)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return current.StartTicks == expected.StartTicks, nil
}
