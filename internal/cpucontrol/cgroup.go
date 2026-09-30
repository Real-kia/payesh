package cpucontrol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

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
// requires a disposable Linux host.
type CgroupFS interface {
	EnsureDedicatedGroup(group GroupPath) error
	IsDedicatedGroup(group GroupPath) (bool, error)
	ReadQuota(group GroupPath) (millicores uint64, unlimited bool, err error)
	WriteQuota(group GroupPath, millicores uint64, unlimited bool) error
	AttachProcess(group GroupPath, pid int) error
	ContainsProcess(group GroupPath, pid int) (bool, error)
	ParentQuota(group GroupPath) (millicores uint64, unlimited bool, hasParent bool, err error)
	IsEmpty(group GroupPath) (bool, error)
	RemoveGroup(group GroupPath) error
}

// FSCgroup implements CgroupFS against Root, the cgroup v2 mount point in
// production (conventionally /sys/fs/cgroup) or a temp directory in tests.
type FSCgroup struct {
	Root string
	// OwnershipRoot is an ordinary filesystem directory, never part of the
	// cgroup pseudo-filesystem. cgroupfs does not allow applications to create
	// arbitrary marker files inside cgroup directories.
	OwnershipRoot string
	PeriodMicros  uint64
	// AllowPlainFilesystem enables removal of literal test fixture files. It
	// must stay false for a real cgroupfs mount, where only rmdir is appropriate.
	AllowPlainFilesystem bool
}

func (c FSCgroup) ownershipPath(group GroupPath) (string, error) {
	if err := group.validate(); err != nil {
		return "", err
	}
	if c.OwnershipRoot == "" {
		return "", errors.New("cpucontrol: ownership metadata root is not configured")
	}
	sum := sha256.Sum256([]byte(group))
	return filepath.Join(c.OwnershipRoot, fmt.Sprintf("%x.owned", sum[:])), nil
}

func directoryIdentity(path string, info os.FileInfo) (uint64, uint64, int64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, errors.New("cpucontrol: filesystem identity is unavailable")
	}
	// Filesystems may immediately recycle an inode after rmdir. Binding the
	// ownership record to creation time as well prevents a recreated path from
	// inheriting authorization solely because dev+ino were reused. Unlike mtime
	// or ctime, birth time is stable when child cgroups and control files change.
	createdAt, err := directoryBirthNanos(path, info)
	if err != nil {
		return 0, 0, 0, err
	}
	return uint64(stat.Dev), uint64(stat.Ino), createdAt, nil
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
// records ownership outside cgroupfs. It refuses to "adopt" a directory that
// already exists without that durable record — the unsafe shared-group case
// design requires refusing.
func (c FSCgroup) EnsureDedicatedGroup(group GroupPath) error {
	dir, err := c.dir(group)
	if err != nil {
		return err
	}
	if group != GroupPath("payesh") {
		// Never create a managed child below a pre-existing unowned `payesh`
		// directory. Recursing through the ownership boundary either creates and
		// stamps our parent or refuses the foreign hierarchy.
		if err := c.EnsureDedicatedGroup(GroupPath("payesh")); err != nil {
			return err
		}
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
	marker, err := c.ownershipPath(group)
	if err != nil {
		_ = os.Remove(dir)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		_ = os.Remove(dir)
		return fmt.Errorf("cpucontrol: prepare ownership metadata: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		_ = os.Remove(dir)
		return fmt.Errorf("cpucontrol: inspect created cgroup: %w", err)
	}
	device, inode, createdAt, err := directoryIdentity(dir, info)
	if err != nil {
		_ = os.Remove(dir)
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = c.removeUnownedCreatedDir(dir)
		return fmt.Errorf("cpucontrol: generate ownership token: %w", err)
	}
	token := hex.EncodeToString(nonce[:])
	if err := attachDirectoryOwnership(dir, token, c.AllowPlainFilesystem); err != nil {
		_ = c.removeUnownedCreatedDir(dir)
		return fmt.Errorf("cpucontrol: attach cgroup ownership: %w", err)
	}
	record := fmt.Sprintf("%s\n%d\n%d\n%d\n%s\n", group, device, inode, createdAt, token)
	if err := os.WriteFile(marker, []byte(record), 0o600); err != nil {
		_ = os.Remove(dir)
		return fmt.Errorf("cpucontrol: stamp cgroup ownership: %w", err)
	}
	return nil
}

func (c FSCgroup) IsDedicatedGroup(group GroupPath) (bool, error) {
	dir, err := c.dir(group)
	if err != nil {
		return false, err
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, statErr
	} else if !info.IsDir() {
		return false, nil
	}
	marker, err := c.ownershipPath(group)
	if err != nil {
		return false, err
	}
	raw, statErr := os.ReadFile(marker)
	if statErr == nil {
		var recordedGroup string
		var recordedDevice, recordedInode uint64
		var recordedCreatedAt int64
		var recordedToken string
		if _, err := fmt.Sscanf(string(raw), "%s\n%d\n%d\n%d\n%s", &recordedGroup, &recordedDevice, &recordedInode, &recordedCreatedAt, &recordedToken); err != nil {
			return false, nil
		}
		device, inode, createdAt, err := directoryIdentity(dir, info)
		if err != nil {
			return false, err
		}
		attachedToken, err := readDirectoryOwnership(dir, c.AllowPlainFilesystem)
		if err != nil {
			return false, nil
		}
		return recordedGroup == string(group) && recordedDevice == device && recordedInode == inode && recordedCreatedAt == createdAt && recordedToken == attachedToken, nil
	} else if os.IsNotExist(statErr) {
		return false, nil
	} else {
		return false, statErr
	}
}

func (c FSCgroup) removeUnownedCreatedDir(dir string) error {
	if c.AllowPlainFilesystem {
		return os.RemoveAll(dir)
	}
	return os.Remove(dir)
}

func (c FSCgroup) AttachProcess(group GroupPath, pid int) error {
	if pid <= 0 {
		return errors.New("cpucontrol: pid must be positive")
	}
	dir, err := c.dir(group)
	if err != nil {
		return err
	}
	owned, err := c.IsDedicatedGroup(group)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("cpucontrol: refusing to attach a process to a cgroup Payesh does not own: %s", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("cpucontrol: attach process to cgroup: %w", err)
	}
	return nil
}

func (c FSCgroup) ContainsProcess(group GroupPath, pid int) (bool, error) {
	dir, err := c.dir(group)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return false, err
	}
	want := strconv.Itoa(pid)
	for _, field := range strings.Fields(string(raw)) {
		if field == want {
			return true, nil
		}
	}
	return false, nil
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

// EnsureCPUHierarchy creates the Payesh-owned parent and verifies that the
// kernel has delegated the CPU controller into both the cgroup root and the
// Payesh subtree.  Enabling a controller is intentionally explicit: a
// directory existing under /sys/fs/cgroup does not imply that cpu.max will be
// available or enforced.  A failure leaves the runtime unavailable rather
// than allowing a policy to be reported as applied against a plain directory.
func (c FSCgroup) EnsureCPUHierarchy() error {
	if !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) != c.Root {
		return errors.New("cpucontrol: cgroup root must be a clean absolute path")
	}
	if err := c.ensureController(c.Root); err != nil {
		return err
	}
	if err := c.EnsureDedicatedGroup(GroupPath("payesh")); err != nil {
		return err
	}
	payeshDir, err := c.dir(GroupPath("payesh"))
	if err != nil {
		return err
	}
	if err := c.ensureController(payeshDir); err != nil {
		return err
	}
	return nil
}

func (c FSCgroup) ensureController(dir string) error {
	controllers, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cpucontrol: read delegated controllers at %s: %w", dir, err)
	}
	if !containsController(string(controllers), "cpu") {
		return fmt.Errorf("cpucontrol: cpu controller is unavailable at %s", dir)
	}
	subtreePath := filepath.Join(dir, "cgroup.subtree_control")
	subtree, err := os.ReadFile(subtreePath)
	if err != nil {
		return fmt.Errorf("cpucontrol: read subtree controllers at %s: %w", dir, err)
	}
	if containsController(string(subtree), "cpu") {
		return nil
	}
	if err := os.WriteFile(subtreePath, []byte("+cpu\n"), 0o644); err != nil {
		return fmt.Errorf("cpucontrol: delegate cpu controller at %s: %w", dir, err)
	}
	readBack, err := os.ReadFile(subtreePath)
	if err != nil {
		return fmt.Errorf("cpucontrol: verify subtree controllers at %s: %w", dir, err)
	}
	if !containsController(string(readBack), "cpu") {
		return fmt.Errorf("cpucontrol: cpu controller delegation was not retained at %s", dir)
	}
	return nil
}

func containsController(value, want string) bool {
	for _, field := range strings.Fields(value) {
		if strings.TrimPrefix(field, "+") == want {
			return true
		}
	}
	return false
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
	if err := os.Remove(dir); err != nil {
		if !c.AllowPlainFilesystem {
			return fmt.Errorf("cpucontrol: remove cgroup: %w", err)
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("cpucontrol: remove fixture cgroup: %w", err)
		}
	}
	marker, err := c.ownershipPath(group)
	if err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cpucontrol: remove ownership metadata: %w", err)
	}
	return nil
}
