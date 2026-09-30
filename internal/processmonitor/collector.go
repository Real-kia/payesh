// Package processmonitor belongs exclusively to the optional process-monitoring executable.
package processmonitor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const MaxProcesses = 32768

type Process struct {
	PID                           int      `json:"pid"`
	Name                          string   `json:"name"`
	UID                           *uint64  `json:"uid"`
	State                         string   `json:"state"`
	Threads                       uint64   `json:"threads"`
	MemoryBytes                   uint64   `json:"memory_bytes,string"`
	CPUPercent                    *float64 `json:"cpu_percent"`
	ReadBytesPerSecond            *float64 `json:"read_bytes_per_second"`
	WriteBytesPerSecond           *float64 `json:"write_bytes_per_second"`
	Connections                   *int     `json:"connections"`
	ticks, started, read, written uint64
	ioOK                          bool
}

type Snapshot struct {
	SampledAt         time.Time `json:"sampled_at"`
	IntervalSeconds   float64   `json:"interval_seconds"`
	Total             int       `json:"total"`
	Truncated         bool      `json:"truncated"`
	Items             []Process `json:"items"`
	NetworkAccounting string    `json:"network_accounting"`
}

type Collector struct {
	Root       string
	PageSize   uint64
	previous   map[int]Process
	totalTicks uint64
	sampledAt  time.Time
}

func NewCollector(root string) *Collector {
	return &Collector{Root: root, PageSize: uint64(os.Getpagesize())}
}

// Linux aggregate CPU accounting avoids assuming a particular USER_HZ. CPU is
// expressed as percent of one core, so a multithreaded process can exceed 100%.
func (c *Collector) Sample(ctx context.Context, now time.Time) (Snapshot, error) {
	data, err := readBounded(filepath.Join(c.Root, "stat"), 256<<10)
	if err != nil {
		return Snapshot{}, err
	}
	lines := strings.Split(string(data), "\n")
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return Snapshot{}, errors.New("invalid aggregate CPU counters")
	}
	var total uint64
	// guest and guest_nice are already included in user and nice.
	for i := 1; i < len(fields) && i <= 8; i++ {
		v, e := strconv.ParseUint(fields[i], 10, 64)
		if e != nil {
			return Snapshot{}, e
		}
		total += v
	}
	cores := 0
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], "cpu") && f[0] != "cpu" {
			cores++
		}
	}
	if cores == 0 {
		cores = 1
	}
	dir, err := os.Open(c.Root)
	if err != nil {
		return Snapshot{}, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(MaxProcesses + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Snapshot{}, err
	}
	snapshot := Snapshot{SampledAt: now.UTC(), Items: make([]Process, 0), NetworkAccounting: "connections-only", Truncated: len(names) > MaxProcesses}
	if len(names) > MaxProcesses {
		names = names[:MaxProcesses]
	}
	elapsed := now.Sub(c.sampledAt).Seconds()
	if !c.sampledAt.IsZero() && elapsed > 0 {
		snapshot.IntervalSeconds = elapsed
	}
	next := make(map[int]Process)
	fdBudget := 65536
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		pid, e := strconv.Atoi(name)
		if e != nil || pid <= 0 {
			continue
		}
		path := filepath.Join(c.Root, name)
		raw, e := readBounded(filepath.Join(path, "stat"), 8192)
		if e != nil {
			continue
		}
		p, e := parseStat(raw, c.PageSize)
		if e != nil || p.PID != pid {
			continue
		}
		if status, e := readBounded(filepath.Join(path, "status"), 64<<10); e == nil {
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, "Uid:") {
					f := strings.Fields(line)
					if len(f) > 1 {
						uid, e := strconv.ParseUint(f[1], 10, 64)
						if e == nil {
							p.UID = &uid
						}
					}
				}
			}
		}
		if raw, e := readBounded(filepath.Join(path, "io"), 8192); e == nil {
			p.read, p.written, p.ioOK = parseIO(raw)
		}
		if fdBudget > 0 {
			p.Connections = countConnections(filepath.Join(path, "fd"), &fdBudget)
		}
		// Re-read identity after ancillary files: a PID may disappear or be reused
		// during the scan, which must not combine counters from different processes.
		check, e := readBounded(filepath.Join(path, "stat"), 8192)
		if e != nil {
			continue
		}
		identity, e := parseStat(check, c.PageSize)
		if e != nil || identity.started != p.started {
			continue
		}
		previous, ok := c.previous[pid]
		if ok && previous.started == p.started && elapsed > 0 && !c.sampledAt.IsZero() {
			if total > c.totalTicks && p.ticks >= previous.ticks {
				v := float64(p.ticks-previous.ticks) / float64(total-c.totalTicks) * float64(cores) * 100
				p.CPUPercent = &v
			}
			if p.ioOK && previous.ioOK && p.read >= previous.read && p.written >= previous.written {
				r := float64(p.read-previous.read) / elapsed
				w := float64(p.written-previous.written) / elapsed
				p.ReadBytesPerSecond = &r
				p.WriteBytesPerSecond = &w
			}
		}
		next[pid] = p
		snapshot.Items = append(snapshot.Items, p)
	}
	sort.Slice(snapshot.Items, func(i, j int) bool { return snapshot.Items[i].PID < snapshot.Items[j].PID })
	snapshot.Total = len(snapshot.Items)
	c.previous = next
	c.totalTicks = total
	c.sampledAt = now
	return snapshot, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("proc file exceeds limit")
	}
	return b, e
}

func parseStat(raw []byte, pageSize uint64) (Process, error) {
	text := string(raw)
	open := strings.IndexByte(text, '(')
	close := strings.LastIndexByte(text, ')')
	if open < 1 || close <= open {
		return Process{}, errors.New("invalid process stat")
	}
	pid, e := strconv.Atoi(strings.TrimSpace(text[:open]))
	if e != nil {
		return Process{}, e
	}
	f := strings.Fields(text[close+1:])
	if len(f) < 22 {
		return Process{}, errors.New("short process stat")
	}
	value := func(index int) (uint64, error) { return strconv.ParseUint(f[index], 10, 64) }
	user, e := value(11)
	if e != nil {
		return Process{}, e
	}
	system, e := value(12)
	if e != nil {
		return Process{}, e
	}
	threads, e := value(17)
	if e != nil {
		return Process{}, e
	}
	start, e := value(19)
	if e != nil {
		return Process{}, e
	}
	rss, e := strconv.ParseInt(f[21], 10, 64)
	if e != nil || rss < 0 {
		return Process{}, errors.New("invalid process memory")
	}
	if uint64(rss) > ^uint64(0)/pageSize {
		return Process{}, errors.New("process memory overflow")
	}
	return Process{PID: pid, Name: strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, text[open+1:close]), State: f[0], Threads: threads, MemoryBytes: uint64(rss) * pageSize, ticks: user + system, started: start}, nil
}

func parseIO(raw []byte) (uint64, uint64, bool) {
	var read, write uint64
	var haveRead, haveWrite bool
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) != 2 {
			continue
		}
		v, e := strconv.ParseUint(f[1], 10, 64)
		if e != nil {
			continue
		}
		switch f[0] {
		case "read_bytes:":
			read = v
			haveRead = true
		case "write_bytes:":
			write = v
			haveWrite = true
		}
	}
	return read, write, haveRead && haveWrite
}

func countConnections(path string, budget *int) *int {
	dir, e := os.Open(path)
	if e != nil {
		return nil
	}
	defer dir.Close()
	limit := *budget
	if limit > 4096 {
		limit = 4096
	}
	names, e := dir.Readdirnames(limit + 1)
	if e != nil && !errors.Is(e, io.EOF) {
		return nil
	}
	if len(names) > limit {
		*budget -= limit
		return nil
	}
	*budget -= len(names)
	count := 0
	for _, name := range names {
		target, e := os.Readlink(filepath.Join(path, name))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e == nil && strings.HasPrefix(target, "socket:[") {
			count++
		}
	}
	return &count
}

func Sort(items []Process, by string) error {
	switch by {
	case "cpu", "memory", "read", "write", "connections", "pid":
	default:
		return fmt.Errorf("invalid sort: %s", by)
	}
	score := func(p Process) float64 {
		switch by {
		case "memory":
			return float64(p.MemoryBytes)
		case "cpu":
			if p.CPUPercent != nil {
				return *p.CPUPercent
			}
		case "read":
			if p.ReadBytesPerSecond != nil {
				return *p.ReadBytesPerSecond
			}
		case "write":
			if p.WriteBytesPerSecond != nil {
				return *p.WriteBytesPerSecond
			}
		case "connections":
			if p.Connections != nil {
				return float64(*p.Connections)
			}
		}
		return -1
	}
	sort.Slice(items, func(i, j int) bool {
		if by == "pid" {
			return items[i].PID < items[j].PID
		}
		a, b := score(items[i]), score(items[j])
		if a == b {
			return items[i].PID < items[j].PID
		}
		return a > b
	})
	return nil
}
