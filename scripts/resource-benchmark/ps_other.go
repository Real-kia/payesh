//go:build !linux

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func collectPlatformSnapshot(root int) (snapshot, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=,%cpu=").Output()
	if err != nil {
		return snapshot{}, err
	}
	type row struct {
		pid, ppid int
		rss       uint64
		cpu       float64
	}
	rows := map[int]row{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		rss, e3 := strconv.ParseUint(f[2], 10, 64)
		cpu, e4 := strconv.ParseFloat(f[3], 64)
		if e1 == nil && e2 == nil && e3 == nil && e4 == nil {
			rows[pid] = row{pid, ppid, rss * 1024, cpu}
		}
	}
	if _, ok := rows[root]; !ok {
		return snapshot{}, fmt.Errorf("root process %d is not visible", root)
	}
	children := map[int][]int{}
	for pid, r := range rows {
		children[r.ppid] = append(children[r.ppid], pid)
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
		r, ok := rows[pid]
		if !ok {
			continue
		}
		count++
		total.RSSBytes += r.rss
		queue = append(queue, children[pid]...)
	}
	return snapshot{Metric: total, ProcessCount: count}, nil
}

func clockTicks() uint64 { return 100 }
