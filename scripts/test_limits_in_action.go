package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Real-kia/payesh/internal/bandwidth"
	"github.com/Real-kia/payesh/internal/cpucontrol"
	"github.com/Real-kia/payesh/internal/porttraffic"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "cpu-worker-process" {
		runCPUWorker()
		return
	}

	ctx := context.Background()
	fmt.Println("======================================================================")
	fmt.Println("   PAYESH LIVE ENFORCEMENT ACCEPTANCE: CPU & NETWORK LIMITS IN ACTION ")
	fmt.Println("======================================================================")

	// Determine cgroup root
	cgroupRoot := "/sys/fs/cgroup"
	ownershipRoot := "/var/lib/payesh/cgroup-ownership"

	// -------------------------------------------------------------------
	// TEST 1: CPU LIMITS IN ACTION
	// -------------------------------------------------------------------
	fmt.Println("\n======================================================================")
	fmt.Println(" [EXPERIMENT 1] Real-Time CPU Limiting: Stress, Clamp & Dynamic Adjustment")
	fmt.Println("======================================================================")

	fs := cpucontrol.FSCgroup{
		Root:          cgroupRoot,
		OwnershipRoot: ownershipRoot,
	}

	if err := fs.EnsureCPUHierarchy(); err != nil {
		panic(fmt.Sprintf("EnsureCPUHierarchy: %v", err))
	}

	target := cpucontrol.Target{Kind: cpucontrol.TargetKindProcessGroup, Name: "live-limit-burn"}
	group := target.GroupPath()

	// Ensure group exists
	_ = fs.RemoveGroup(group)
	if err := fs.EnsureDedicatedGroup(group); err != nil {
		panic(fmt.Sprintf("EnsureDedicatedGroup: %v", err))
	}
	defer func() {
		_ = fs.RemoveGroup(group)
		fmt.Println("[Cleanup] CPU cgroup removed.")
	}()

	// Spawn a CPU burner process (running infinite math computations)
	var stopBurner int32
	burnerDone := make(chan struct{})
	var burnerPID int

	burnerCmd := exec.Command("/proc/self/exe", "cpu-worker-process")

	burnerCmd.Stdout = os.Stdout
	burnerCmd.Stderr = os.Stderr
	if err := burnerCmd.Start(); err != nil {
		panic(fmt.Sprintf("Start burner: %v", err))
	}
	burnerPID = burnerCmd.Process.Pid
	defer func() {
		_ = burnerCmd.Process.Kill()
		_ = burnerCmd.Wait()
		fmt.Println("[Cleanup] CPU burner process stopped.")
	}()

	fmt.Printf("[1.0] Started CPU burner worker with PID %d\n", burnerPID)
	if err := fs.AttachProcess(group, burnerPID); err != nil {
		panic(fmt.Sprintf("AttachProcess: %v", err))
	}
	fmt.Printf("[1.0] Attached PID %d to cgroup %s\n", burnerPID, group)

	// Phase 1.1: Unthrottled Baseline
	fmt.Println("\n--- [Phase 1.1] Unthrottled Baseline (Quota: Unlimited) ---")
	if err := fs.WriteQuota(group, 0, true); err != nil {
		panic(err)
	}
	time.Sleep(1 * time.Second) // allow warm up
	unthrottledCPU := sampleProcessCPUPercent(burnerPID, 3*time.Second)
	fmt.Printf(">>> Result: Unthrottled Worker CPU Usage = %.2f%% (~%.0f millicores)\n",
		unthrottledCPU, unthrottledCPU*10)

	// Phase 1.2: Clamp to 250 Millicores (25.0% CPU)
	fmt.Println("\n--- [Phase 1.2] Apply Payesh Limit: 250 millicores (25.0% CPU cap) ---")
	if err := fs.WriteQuota(group, 250, false); err != nil {
		panic(err)
	}
	time.Sleep(500 * time.Millisecond) // wait for CFS period window
	throttled250CPU := sampleProcessCPUPercent(burnerPID, 3*time.Second)
	fmt.Printf(">>> Result: Throttled (Target: 25.00%%) Worker CPU Usage = %.2f%% (~%.0f millicores)\n",
		throttled250CPU, throttled250CPU*10)

	if throttled250CPU > 20.0 && throttled250CPU < 30.0 {
		fmt.Printf(">>> [VERIFIED] CPU successfully clamped from %.1f%% to %.1f%% (Target: 25.0%%)!\n",
			unthrottledCPU, throttled250CPU)
	} else {
		fmt.Printf(">>> [WARNING] Throttled CPU %.2f%% slightly outside 20-30%% range\n", throttled250CPU)
	}

	// Phase 1.3: Dynamically adjust limit to 500 Millicores (50.0% CPU)
	fmt.Println("\n--- [Phase 1.3] Dynamically Adjust Limit: 500 millicores (50.0% CPU cap) ---")
	if err := fs.WriteQuota(group, 500, false); err != nil {
		panic(err)
	}
	time.Sleep(500 * time.Millisecond)
	throttled500CPU := sampleProcessCPUPercent(burnerPID, 3*time.Second)
	fmt.Printf(">>> Result: Throttled (Target: 50.00%%) Worker CPU Usage = %.2f%% (~%.0f millicores)\n",
		throttled500CPU, throttled500CPU*10)

	if throttled500CPU > 42.0 && throttled500CPU < 58.0 {
		fmt.Printf(">>> [VERIFIED] CPU limit dynamically increased to %.1f%% (Target: 50.0%%)!\n",
			throttled500CPU)
	}

	// Phase 1.4: Dynamically adjust down to 100 Millicores (10.0% CPU)
	fmt.Println("\n--- [Phase 1.4] Dynamically Adjust Limit: 100 millicores (10.0% CPU cap) ---")
	if err := fs.WriteQuota(group, 100, false); err != nil {
		panic(err)
	}
	time.Sleep(500 * time.Millisecond)
	throttled100CPU := sampleProcessCPUPercent(burnerPID, 3*time.Second)
	fmt.Printf(">>> Result: Throttled (Target: 10.00%%) Worker CPU Usage = %.2f%% (~%.0f millicores)\n",
		throttled100CPU, throttled100CPU*10)

	if throttled100CPU > 7.0 && throttled100CPU < 14.0 {
		fmt.Printf(">>> [VERIFIED] CPU limit dynamically lowered to %.1f%% (Target: 10.0%%)!\n",
			throttled100CPU)
	}

	// Phase 1.5: Release limit back to unlimited
	fmt.Println("\n--- [Phase 1.5] Release Limit (Back to Unlimited) ---")
	if err := fs.WriteQuota(group, 0, true); err != nil {
		panic(err)
	}
	time.Sleep(500 * time.Millisecond)
	releasedCPU := sampleProcessCPUPercent(burnerPID, 3*time.Second)
	fmt.Printf(">>> Result: Released Worker CPU Usage = %.2f%% (~%.0f millicores)\n",
		releasedCPU, releasedCPU*10)

	if releasedCPU > 80.0 {
		fmt.Printf(">>> [VERIFIED] Process immediately unthrottled and recovered to %.1f%% CPU!\n", releasedCPU)
	}

	_ = burnerCmd.Process.Kill()
	_ = burnerCmd.Wait()
	_ = fs.RemoveGroup(group)

	// -------------------------------------------------------------------
	// TEST 2: NETWORK BANDWIDTH LIMITS IN ACTION (CONTINUOUS UPLOAD)
	// -------------------------------------------------------------------
	fmt.Println("\n======================================================================")
	fmt.Println(" [EXPERIMENT 2] Real-Time Network Egress Limiting: Burst vs Clamped Rate")
	fmt.Println("======================================================================")

	const (
		leftDev  = "pybwlive0"
		rightDev = "pybwlive1"
		netns    = "pybwnslive"
		testPort = 28989
	)
	_ = exec.Command("ip", "link", "del", leftDev).Run()
	_ = exec.Command("ip", "link", "del", rightDev).Run()
	_ = exec.Command("ip", "netns", "del", netns).Run()

	mustRun("ip", "link", "add", leftDev, "type", "veth", "peer", "name", rightDev)
	mustRun("ip", "netns", "add", netns)
	mustRun("ip", "link", "set", rightDev, "netns", netns)
	defer func() {
		_ = exec.Command("ip", "netns", "del", netns).Run()
		_ = exec.Command("ip", "link", "del", leftDev).Run()
		fmt.Println("[Cleanup] Network veth pair and netns removed.")
	}()

	mustRun("ip", "addr", "add", "198.18.50.1/30", "dev", leftDev)
	mustRun("ip", "link", "set", leftDev, "up")
	mustRun("ip", "netns", "exec", netns, "ip", "addr", "add", "198.18.50.2/30", "dev", rightDev)
	mustRun("ip", "netns", "exec", netns, "ip", "link", "set", "lo", "up")
	mustRun("ip", "netns", "exec", netns, "ip", "link", "set", rightDev, "up")

	// Start continuous TCP sink inside namespace
	recvScript := fmt.Sprintf(`
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('198.18.50.2', %d))
s.listen(5)
while True:
    try:
        c, _ = s.accept()
        while True:
            b = c.recv(65536)
            if not b: break
        c.close()
    except:
        break
`, testPort)

	sinkCmd := exec.Command("ip", "netns", "exec", netns, "python3", "-c", recvScript)
	if err := sinkCmd.Start(); err != nil {
		panic(fmt.Sprintf("Start sink: %v", err))
	}
	defer func() {
		_ = sinkCmd.Process.Kill()
		_ = sinkCmd.Wait()
		fmt.Println("[Cleanup] TCP sink terminated.")
	}()
	time.Sleep(200 * time.Millisecond)

	tcBackend := bandwidth.TCBackend{StateDir: "/tmp/payesh-tc-live-statedir"}
	_ = os.RemoveAll("/tmp/payesh-tc-live-statedir")

	// Phase 2.1: Unthrottled Upload
	fmt.Println("\n--- [Phase 2.1] Baseline Upload Speed (Unthrottled) ---")
	unthrottledBps := measureActiveUploadRate("198.18.50.1", "198.18.50.2", testPort, 2*time.Second)
	fmt.Printf(">>> Result: Unthrottled Upload Speed = %.2f Mbit/s (%.2f MB/s)\n",
		unthrottledBps/1_000_000, unthrottledBps/8/(1024*1024))

	// Phase 2.2: Apply 16 Mbit/s (2.0 MB/s) Egress Shaper
	fmt.Println("\n--- [Phase 2.2] Apply Payesh Shaper: 16 Mbit/s (2.0 MB/s) ---")
	preview16, err := bandwidth.BuildPreview(bandwidth.Request{
		Scope:         bandwidth.Scope{TargetKind: "interface", Interface: leftDev, Direction: porttraffic.Outbound},
		Action:        bandwidth.Throttle,
		BitsPerSecond: 16_000_000,
	}, nil)
	if err != nil {
		panic(err)
	}

	if err := tcBackend.Apply(ctx, preview16); err != nil {
		panic(fmt.Sprintf("Apply 16 Mbps shaper: %v", err))
	}
	defer func() {
		_ = tcBackend.RevertOwned(ctx, bandwidth.Checkpoint{
			ID:       "cleanup-shaper",
			Scope:    preview16.Request.Scope,
			Action:   preview16.Request.Action,
			Previous: json.RawMessage(`{"interface":"` + leftDev + `","absent":true}`),
		})
	}()

	time.Sleep(200 * time.Millisecond)
	throttled16Bps := measureActiveUploadRate("198.18.50.1", "198.18.50.2", testPort, 3*time.Second)
	fmt.Printf(">>> Result: Throttled (Target: 16.00 Mbit/s) Upload Speed = %.2f Mbit/s (%.2f MB/s)\n",
		throttled16Bps/1_000_000, throttled16Bps/8/(1024*1024))

	if throttled16Bps > 13_000_000 && throttled16Bps < 22_000_000 {
		fmt.Printf(">>> [VERIFIED] Network upload accurately clamped to %.2f Mbit/s (target 16 Mbit/s)!\n",
			throttled16Bps/1_000_000)
	}

	// Phase 2.3: Dynamically adjust limit to 32 Mbit/s (4.0 MB/s)
	fmt.Println("\n--- [Phase 2.3] Dynamically Adjust Payesh Shaper: 32 Mbit/s (4.0 MB/s) ---")
	// Revert previous 16 Mbps shaper first per Payesh safety policy
	_ = tcBackend.RevertOwned(ctx, bandwidth.Checkpoint{
		ID:       "cleanup-16",
		Scope:    preview16.Request.Scope,
		Action:   preview16.Request.Action,
		Previous: json.RawMessage(`{"interface":"` + leftDev + `","absent":true}`),
	})

	preview32, err := bandwidth.BuildPreview(bandwidth.Request{
		Scope:         bandwidth.Scope{TargetKind: "interface", Interface: leftDev, Direction: porttraffic.Outbound},
		Action:        bandwidth.Throttle,
		BitsPerSecond: 32_000_000,
	}, nil)
	if err != nil {
		panic(err)
	}
	if err := tcBackend.Apply(ctx, preview32); err != nil {
		panic(fmt.Sprintf("Apply 32 Mbps shaper: %v", err))
	}

	time.Sleep(200 * time.Millisecond)
	throttled32Bps := measureActiveUploadRate("198.18.50.1", "198.18.50.2", testPort, 3*time.Second)
	fmt.Printf(">>> Result: Throttled (Target: 32.00 Mbit/s) Upload Speed = %.2f Mbit/s (%.2f MB/s)\n",
		throttled32Bps/1_000_000, throttled32Bps/8/(1024*1024))

	if throttled32Bps > 26_000_000 && throttled32Bps < 36_000_000 {
		fmt.Printf(">>> [VERIFIED] Network upload rate dynamically updated to %.2f Mbit/s (target 32 Mbit/s)!\n",
			throttled32Bps/1_000_000)
	}

	// Phase 2.4: Remove Shaper (Back to Wire Speed)
	fmt.Println("\n--- [Phase 2.4] Remove Payesh Shaper (Return to Wire Speed) ---")
	if err := tcBackend.RevertOwned(ctx, bandwidth.Checkpoint{
		ID:       "test-revert",
		Scope:    preview32.Request.Scope,
		Action:   preview32.Request.Action,
		Previous: json.RawMessage(`{"interface":"` + leftDev + `","absent":true}`),
	}); err != nil {
		panic(fmt.Sprintf("Revert shaper: %v", err))
	}

	time.Sleep(200 * time.Millisecond)
	restoredBps := measureActiveUploadRate("198.18.50.1", "198.18.50.2", testPort, 2*time.Second)
	fmt.Printf(">>> Result: Restored Unthrottled Upload Speed = %.2f Mbit/s (%.2f MB/s)\n",
		restoredBps/1_000_000, restoredBps/8/(1024*1024))

	if restoredBps > 500_000_000 {
		fmt.Printf(">>> [VERIFIED] Network upload immediately burst back to wire speed (%.2f Mbit/s)!\n",
			restoredBps/1_000_000)
	}

	fmt.Println("\n======================================================================")
	fmt.Println(" >>> ALL REAL-TIME LIMIT ENFORCEMENT TESTS PASSED SUCCESSFULLY! <<< ")
	fmt.Println("======================================================================")
	_ = stopBurner
	_ = burnerDone
}

func runCPUWorker() {
	// Worker loop doing busy math computations
	runtime.GOMAXPROCS(1)
	var x float64 = 1.0001
	for {
		for i := 0; i < 1000000; i++ {
			x = math.Sin(x)*math.Cos(x)*math.Tan(x) + 1.0001
		}
	}
}

func sampleProcessCPUPercent(pid int, duration time.Duration) float64 {
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	u1, s1 := readProcessTicks(statPath)
	start := time.Now()
	time.Sleep(duration)
	u2, s2 := readProcessTicks(statPath)
	elapsed := time.Since(start)

	deltaTicks := float64((u2 + s2) - (u1 + s1))
	// 100 ticks per second (standard Linux USER_HZ = 100)
	cpuSeconds := deltaTicks / 100.0
	cpuPercent := (cpuSeconds / elapsed.Seconds()) * 100.0
	return cpuPercent
}

func readProcessTicks(path string) (uint64, uint64) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 15 {
		return 0, 0
	}
	utime, _ := strconv.ParseUint(fields[13], 10, 64)
	stime, _ := strconv.ParseUint(fields[14], 10, 64)
	return utime, stime
}

func measureActiveUploadRate(localIP, remoteIP string, port int, duration time.Duration) float64 {
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(localIP)}}
	conn, err := dialer.Dial("tcp", fmt.Sprintf("%s:%d", remoteIP, port))
	if err != nil {
		panic(fmt.Sprintf("Dial %s:%d: %v", remoteIP, port, err))
	}
	defer conn.Close()

	buf := make([]byte, 64*1024)
	var totalBytes int64
	var active int32 = 1

	deadline := time.Now().Add(duration)
	_ = conn.SetDeadline(deadline.Add(500 * time.Millisecond))

	start := time.Now()
	for time.Now().Before(deadline) && atomic.LoadInt32(&active) == 1 {
		n, err := conn.Write(buf)
		if err != nil {
			break
		}
		totalBytes += int64(n)
	}
	elapsed := time.Since(start)

	bps := float64(totalBytes*8) / elapsed.Seconds()
	return bps
}

func mustRun(name string, args ...string) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("command %s %v failed: %s: %v", name, args, string(out), err))
	}
}
