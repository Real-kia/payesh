//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type procInfo struct {
	pid, ppid int
	metric    processMetric
}

func collectPlatformSnapshot(root int) (snapshot, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return snapshot{}, err
	}
	all := make(map[int]procInfo)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := readProcInfo(pid)
		if err != nil {
			continue
		}
		all[pid] = info
	}
	if _, ok := all[root]; !ok {
		return snapshot{}, fmt.Errorf("root process %d is not visible", root)
	}
	children := make(map[int][]int)
	for pid, info := range all {
		children[info.ppid] = append(children[info.ppid], pid)
	}
	queue := []int{root}
	seen := map[int]bool{}
	var total processMetric
	count := 0
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		info, ok := all[pid]
		if !ok {
			continue
		}
		count++
		total.CPUSeconds += info.metric.CPUSeconds
		total.RSSBytes += info.metric.RSSBytes
		total.IORead += info.metric.IORead
		total.IOWrite += info.metric.IOWrite
		total.IOSupported = total.IOSupported || info.metric.IOSupported
		queue = append(queue, children[pid]...)
	}
	return snapshot{Metric: total, ProcessCount: count}, nil
}

func readProcInfo(pid int) (procInfo, error) {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procInfo{}, err
	}
	close := strings.LastIndexByte(string(stat), ')')
	if close < 0 {
		return procInfo{}, fmt.Errorf("malformed stat")
	}
	fields := strings.Fields(string(stat)[close+2:])
	if len(fields) < 22 {
		return procInfo{}, fmt.Errorf("short stat")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procInfo{}, err
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return procInfo{}, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return procInfo{}, err
	}
	rss, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil || rss < 0 {
		return procInfo{}, fmt.Errorf("invalid rss")
	}
	ticks := clockTicks()
	metric := processMetric{CPUSeconds: float64(utime+stime) / float64(ticks), RSSBytes: uint64(rss) * uint64(os.Getpagesize())}
	if body, ioErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "io")); ioErr == nil {
		var gotRead, gotWrite bool
		for _, line := range strings.Split(string(body), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 2 {
				continue
			}
			n, parseErr := strconv.ParseUint(parts[1], 10, 64)
			if parseErr != nil {
				continue
			}
			switch parts[0] {
			case "read_bytes:":
				metric.IORead = n
				gotRead = true
			case "write_bytes:":
				metric.IOWrite = n
				gotWrite = true
			}
		}
		metric.IOSupported = gotRead && gotWrite
	}
	return procInfo{pid: pid, ppid: ppid, metric: metric}, nil
}

func clockTicks() uint64 {
	// Linux's conventional value is 100, but respect unusual containers/hosts.
	if b, err := execGetconf(); err == nil {
		if n, err := strconv.ParseUint(strings.TrimSpace(b), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 100
}
