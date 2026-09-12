//go:build linux

package cpucontrol

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLinuxCPUControlAcceptance is opt-in because it mutates only a new
// Payesh cgroup below the configured cgroup-v2 root and needs root plus a
// delegated CPU controller. It verifies process placement, quota readback,
// and that a busy workload stays within a bounded CPU budget. Hosts whose
// hierarchy is mounted read-only or has not delegated cpu are reported as
// unsupported rather than being treated as product failures.
func TestLinuxCPUControlAcceptance(t *testing.T) {
	if os.Getenv("PAYESH_LINUX_ACCEPTANCE") != "1" {
		t.Skip("set PAYESH_LINUX_ACCEPTANCE=1 on a disposable Linux host")
	}
	if os.Geteuid() != 0 {
		t.Skip("CPU cgroup acceptance requires root")
	}
	cgroupRoot, err := acceptanceCgroupRoot()
	if err != nil {
		skipCPUCapability(t, err)
	}
	controllers, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		skipCPUCapability(t, err)
	}
	if !containsWord(string(controllers), "cpu") {
		t.Skip("cgroup v2 cpu controller is unavailable")
	}
	subtree, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"))
	if err != nil {
		skipCPUCapability(t, err)
	}
	if !containsWord(string(subtree), "cpu") {
		if os.Getenv("PAYESH_ACCEPTANCE_ENABLE_CPU_CONTROLLER") != "1" {
			t.Skip("cgroup v2 cpu controller is not delegated to child cgroups")
		}
		// This opt-in is supplied only by the disposable systemd Delegate=yes
		// runner. Moving this test process into a private leaf satisfies cgroup
		// v2's no-internal-process rule before enabling cpu for sibling groups.
		runnerLeaf := filepath.Join(cgroupRoot, fmt.Sprintf("payesh-acceptance-runner-%d", os.Getpid()))
		if err := os.Mkdir(runnerLeaf, 0o755); err != nil {
			skipCPUCapability(t, err)
		}
		if err := os.WriteFile(filepath.Join(runnerLeaf, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			skipCPUCapability(t, err)
		}
		if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), []byte("+cpu"), 0o600); err != nil {
			skipCPUCapability(t, err)
		}
		subtree, err = os.ReadFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"))
		if err != nil || !containsWord(string(subtree), "cpu") {
			skipCPUCapability(t, err)
		}
	}

	name := fmt.Sprintf("linux-acceptance-%d", os.Getpid())
	target := Target{Kind: TargetKindProcessGroup, Name: name}
	group := target.GroupPath()
	fs := FSCgroup{Root: cgroupRoot, OwnershipRoot: t.TempDir()}
	if err := fs.EnsureCPUHierarchy(); err != nil {
		skipCPUCapability(t, err)
	}
	groupDir := filepath.Join(cgroupRoot, filepath.FromSlash(string(group)))
	// Exercise the foreign-group safety boundary on the real hierarchy before
	// creating the Payesh-owned group used by the workload probe.
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		skipCPUCapability(t, err)
	}
	if err := fs.EnsureDedicatedGroup(group); err == nil {
		t.Fatalf("accepted a pre-existing unowned cgroup %s", group)
	}
	if err := os.Remove(groupDir); err != nil {
		skipCPUCapability(t, err)
	}
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		skipCPUCapability(t, err)
	}
	defer func() {
		if _, statErr := os.Stat(groupDir); os.IsNotExist(statErr) {
			return
		}
		if err := fs.RemoveGroup(group); err != nil {
			t.Errorf("remove acceptance cgroup: %v", err)
		}
	}()

	if _, err := os.Stat(filepath.Join(groupDir, "cpu.max")); err != nil {
		skipCPUCapability(t, err)
	}
	if _, err := os.Stat(filepath.Join(groupDir, "cpu.stat")); err != nil {
		skipCPUCapability(t, err)
	}
	childControllers, err := os.ReadFile(filepath.Join(groupDir, "cgroup.controllers"))
	if err != nil {
		skipCPUCapability(t, err)
	}
	if !containsWord(string(childControllers), "cpu") {
		t.Skip("child cgroup does not expose the delegated cpu controller")
	}
	// Reconstructing the adapter with the same durable ownership directory is
	// the restart case: ownership must remain verifiable without adopting a
	// path merely because it happens to exist.
	restartedFS := FSCgroup{Root: cgroupRoot, OwnershipRoot: fs.OwnershipRoot}
	owned, err := restartedFS.IsDedicatedGroup(group)
	if err != nil || !owned {
		t.Fatalf("owned cgroup was not recoverable after adapter restart: owned=%t err=%v", owned, err)
	}
	if err := fs.WriteQuota(group, 200, false); err != nil {
		skipCPUCapability(t, err)
	}
	if quota, unlimited, err := fs.ReadQuota(group); err != nil || unlimited || quota != 200 {
		t.Fatalf("quota readback mismatch: quota=%d unlimited=%t err=%v", quota, unlimited, err)
	}

	before, err := readCPUUsage(filepath.Join(groupDir, "cpu.stat"))
	if err != nil {
		skipCPUCapability(t, err)
	}
	workload := exec.Command("sh", "-c", "while :; do :; done")
	if err := workload.Start(); err != nil {
		t.Fatalf("start busy workload: %v", err)
	}
	defer func() {
		if workload.Process != nil {
			_ = workload.Process.Kill()
			_ = workload.Wait()
		}
	}()
	if err := fs.AttachProcess(group, workload.Process.Pid); err != nil {
		_ = workload.Process.Kill()
		_ = workload.Wait()
		skipCPUCapability(t, err)
	}
	if contained, err := fs.ContainsProcess(group, workload.Process.Pid); err != nil || !contained {
		t.Fatalf("workload was not placed in dedicated group: contained=%t err=%v", contained, err)
	}
	time.Sleep(900 * time.Millisecond)
	if err := workload.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop busy workload: %v", err)
	}
	if err := workload.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait busy workload: %v", err)
		}
	}
	after, err := readCPUUsage(filepath.Join(groupDir, "cpu.stat"))
	if err != nil {
		t.Fatalf("read post-workload cpu.stat: %v", err)
	}
	delta := after - before
	// 200m over 900ms is 180ms of CPU. Allow scheduling and period-boundary
	// grace, but reject an unlimited result that consumes nearly a full core.
	if delta == 0 || delta > 500_000 {
		t.Fatalf("CPU quota was not effective: usage delta=%dus for 200m/900ms", delta)
	}
	t.Logf("cpu quota enforcement=PASS usage_delta_us=%d", delta)
}

// acceptanceCgroupRoot uses the caller's delegated cgroup when systemd (or
// another service manager) launches the probe below the unified hierarchy.
// Falling back to the mount root preserves support for containers which
// delegate the hierarchy directly. The path is derived from procfs and cannot
// escape /sys/fs/cgroup.
func acceptanceCgroupRoot() (string, error) {
	const mountRoot = "/sys/fs/cgroup"
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || fields[0] != "0" || fields[1] != "" {
			continue
		}
		relative := strings.TrimPrefix(filepath.Clean("/"+fields[2]), "/")
		root := filepath.Join(mountRoot, relative)
		if root != mountRoot && !strings.HasPrefix(root, mountRoot+string(os.PathSeparator)) {
			return "", fmt.Errorf("current cgroup escapes unified hierarchy: %q", fields[2])
		}
		return root, nil
	}
	return "", errors.New("unified cgroup v2 membership is unavailable")
}

func skipCPUCapability(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Skip("CPU cgroup capability unavailable")
	}
	if os.IsNotExist(err) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EROFS) {
		t.Skipf("CPU cgroup capability unavailable: %v", err)
	}
	t.Fatalf("CPU cgroup acceptance setup: %v", err)
}

func containsWord(value, want string) bool {
	for _, word := range strings.Fields(value) {
		if word == want {
			return true
		}
	}
	return false
}

func readCPUUsage(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			return strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("cpu.stat does not contain usage_usec")
}
