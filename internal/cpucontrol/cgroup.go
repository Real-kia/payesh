package cpucontrol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ownershipMarker is written inside every cgroup directory Payesh creates.
// Its presence is how EnsureDedicatedGroup/IsDedicatedGroup tell "a group
// this process created and may safely write cpu.max/cgroup.procs into" apart
// from a pre-existing shared or third-party-owned cgroup, which must be
// refused rather than silently repurposed.
const ownershipMarker = "payesh.owned"

// GroupPath identifies one cgroup v2 directory relative to the cgroup root,
// always required to live under "payesh/" so a coding mistake elsewhere in
// this package can never resolve to a cgroup Payesh did not create — the
// same "only touch what we own" boundary archive.go enforces for module
// staging.
type GroupPath string

func (g GroupPath) validate() error {
	if g == "" {
		return errors.New("cpucontrol: group path is required")
	}
	cleaned := filepath.Clean(string(g))
	if cleaned != string(g) || strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return fmt.Errorf("cpucontrol: group path %q is not a clean relative path", g)
	}
	if cleaned != "payesh" && !strings.HasPrefix(cleaned, "payesh"+string(filepath.Separator)) {
		return fmt.Errorf("cpucontrol: group path %q must live under the payesh/ subtree", g)
	}
	return nil
}

// CgroupFS is the small cgroup v2 surface this package needs. FSCgroup below
// implements it against a real (or, in tests, temp-directory-rooted)
// cgroupfs; every method operates only on plain text files in the
// conventional cgroup v2 layout, so the parsing/writing logic is exercised
// for real without requiring an actual mounted cgroupfs or root privileges.
// What a temp-directory root cannot prove is kernel enforcement — that
// requires a disposable Linux host (see docs/handoffs/08.md).
type CgroupFS interface {
	EnsureDedicatedGroup(group GroupPath) error
	IsDedicatedGroup(group GroupPath) (bool, error)
	ReadQuota(group GroupPath) (millicores uint64, unlimited bool, err error)
	WriteQuota(group GroupPath, millicores uint64, unlimited bool) error
	ParentQuota(group GroupPath) (millicores uint64, unlimited bool, hasParent bool, err error)
	IsEmpty(group GroupPath) (bool, error)
	RemoveGroup(group GroupPath) error
}

// FSCgroup implements CgroupFS against Root, the cgroup v2 mount point in
// production (conventionally /sys/fs/cgroup) or a temp directory in tests.
type FSCgroup struct {
	Root         string
	PeriodMicros uint64
}

func (c FSCgroup) period() uint64 {
	if c.PeriodMicros == 0 {
		return DefaultPeriodMicros
	}
	return c.PeriodMicros
}

func (c FSCgroup) dir(group GroupPath) (string, error) {
	if err := group.validate(); err != nil {
		return "", err
	}
	return filepath.Join(c.Root, filepath.FromSlash(string(group))), nil
}

// EnsureDedicatedGroup creates group (and its payesh/ parent) if absent and
// stamps it as Payesh-owned. It refuses to "adopt" a directory that already
// exists without our marker — that would be exactly the unsafe shared-group
// case PLAN.md section 10 requires refusing.
func (c FSCgroup) EnsureDedicatedGroup(group GroupPath) error {
	dir, err := c.dir(group)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(dir); statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("cpucontrol: %s exists and is not a directory", dir)
		}
		owned, ownedErr := c.IsDedicatedGroup(group)
		if ownedErr != nil {
			return ownedErr
		}
		if !owned {
			return fmt.Errorf("cpucontrol: %s already exists and is not Payesh-owned; refusing to repurpose a shared cgroup", dir)
		}
		return nil
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cpucontrol: create dedicated cgroup: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ownershipMarker), []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("cpucontrol: stamp cgroup ownership: %w", err)
	}
	return nil
}

func (c FSCgroup) IsDedicatedGroup(group GroupPath) (bool, error) {
	dir, err := c.dir(group)
	if err != nil {
		return false, err
	}
	if _, statErr := os.Stat(filepath.Join(dir, ownershipMarker)); statErr == nil {
		return true, nil
	} else if os.IsNotExist(statErr) {
		return false, nil
	} else {
		return false, statErr
	}
}

// ReadQuota parses cpu.max, whose content is either "max <period>" (no
// limit) or "<quota> <period>" in microseconds, per the kernel cgroup v2
// documentation.
func (c FSCgroup) ReadQuota(group GroupPath) (uint64, bool, error) {
	dir, err := c.dir(group)
	if err != nil {
		return 0, false, err
	}
	return readCPUMax(filepath.Join(dir, "cpu.max"))
}

func readCPUMax(path string) (uint64, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, true, nil
		}
		return 0, false, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return 0, false, fmt.Errorf("cpucontrol: cpu.max at %s has an unexpected format", path)
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("cpucontrol: cpu.max period at %s is invalid: %w", path, err)
	}
	if fields[0] == "max" {
		return 0, true, nil
	}
	quota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("cpucontrol: cpu.max quota at %s is invalid: %w", path, err)
	}
	return MillicoresFromQuota(quota, period), false, nil
}

// WriteQuota requires the group to already be Payesh-owned (created via
// EnsureDedicatedGroup) so a caller cannot accidentally point this at an
// arbitrary cgroup path.
func (c FSCgroup) WriteQuota(group GroupPath, millicores uint64, unlimited bool) error {
	dir, err := c.dir(group)
	if err != nil {
		return err
	}
	owned, err := c.IsDedicatedGroup(group)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("cpucontrol: refusing to write cpu.max into a cgroup Payesh does not own: %s", dir)
	}
	period := c.period()
	var line string
	if unlimited {
		line = fmt.Sprintf("max %d\n", period)
	} else {
		if err := ValidateMillicores(millicores); err != nil {
			return err
		}
		line = fmt.Sprintf("%d %d\n", QuotaMicros(millicores, period), period)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(line), 0o644); err != nil {
		return fmt.Errorf("cpucontrol: write cpu.max: %w", err)
	}
	return nil
}

// ParentQuota walks up from group's immediate parent to the payesh/ root
// looking for the tightest ancestor limit, so a caller can detect and report
// an inherited stricter quota instead of silently promising an allowance the
// kernel will never actually grant.
func (c FSCgroup) ParentQuota(group GroupPath) (uint64, bool, bool, error) {
	dir, err := c.dir(group)
	if err != nil {
		return 0, false, false, err
	}
	parent := filepath.Dir(dir)
	root := filepath.Clean(c.Root)
	tightestMillicores := uint64(0)
	tightestSet := false
	found := false
	for {
		if parent == root || len(parent) < len(root) {
			break
		}
		millicores, unlimited, readErr := readCPUMax(filepath.Join(parent, "cpu.max"))
		if readErr != nil {
			return 0, false, false, readErr
		}
		if !unlimited {
			found = true
			if !tightestSet || millicores < tightestMillicores {
				tightestMillicores, tightestSet = millicores, true
			}
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	return tightestMillicores, !tightestSet, found, nil
}

func (c FSCgroup) IsEmpty(group GroupPath) (bool, error) {
	dir, err := c.dir(group)
	if err != nil {
		return false, err
	}
	raw, readErr := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return true, nil
		}
		return false, readErr
	}
	return len(strings.TrimSpace(string(raw))) == 0, nil
}

// RemoveGroup deletes a Payesh-owned, empty dedicated group. It refuses to
// remove anything else, including a group that still has member processes —
// the kernel would refuse an rmdir on a non-empty cgroup anyway, but this
// keeps the failure message specific instead of a raw filesystem error.
func (c FSCgroup) RemoveGroup(group GroupPath) error {
	dir, err := c.dir(group)
	if err != nil {
		return err
	}
	owned, err := c.IsDedicatedGroup(group)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("cpucontrol: refusing to remove a cgroup Payesh does not own: %s", dir)
	}
	empty, err := c.IsEmpty(group)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("cpucontrol: cgroup %s still has member processes", dir)
	}
	// RemoveAll rather than Remove: real cgroupfs directories only ever
	// contain kernel-provided virtual files (cpu.max, cgroup.procs, our
	// ownership marker) once IsEmpty has confirmed no member processes
	// remain, so nothing "real" is lost here — this exists because a plain
	// filesystem stand-in (used in tests, and a documented limitation on
	// this macOS development host) represents those virtual files as
	// literal ones that a bare rmdir would otherwise refuse to remove.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cpucontrol: remove cgroup: %w", err)
	}
	return nil
}
