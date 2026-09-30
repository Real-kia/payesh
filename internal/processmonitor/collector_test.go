package processmonitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if e := os.MkdirAll(filepath.Join(root, "42", "fd"), 0700); e != nil {
		t.Fatal(e)
	}
	return root
}
func write(t *testing.T, path, text string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
}
func stat(pid int, name string, ticks, start, rss uint64) string {
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[11] = fmt.Sprint(ticks)
	fields[17] = "2"
	fields[19] = fmt.Sprint(start)
	fields[21] = fmt.Sprint(rss)
	return fmt.Sprintf("%d (%s) %s\n", pid, name, strings.Join(fields, " "))
}
func counters(t *testing.T, root string, total, ticks, start, read, writeBytes uint64) {
	t.Helper()
	write(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu %d 0 0 0 0 0 0 0 999 999\ncpu0 0\ncpu1 0\n", total))
	write(t, filepath.Join(root, "42", "stat"), stat(42, "worker (pool)", ticks, start, 100))
	write(t, filepath.Join(root, "42", "status"), "Uid:\t1000\t1000\t1000\t1000\n")
	write(t, filepath.Join(root, "42", "io"), fmt.Sprintf("read_bytes: %d\nwrite_bytes: %d\n", read, writeBytes))
}
func TestRatesIdentityAndPermissions(t *testing.T) {
	root := fixture(t)
	c := NewCollector(root)
	c.PageSize = 4096
	now := time.Now()
	counters(t, root, 1000, 10, 50, 100, 200)
	if e := os.Symlink("socket:[123]", filepath.Join(root, "42", "fd", "3")); e != nil {
		t.Fatal(e)
	}
	first, e := c.Sample(context.Background(), now)
	if e != nil {
		t.Fatal(e)
	}
	p := first.Items[0]
	if p.CPUPercent != nil || p.ReadBytesPerSecond != nil || p.MemoryBytes != 409600 || p.Name != "worker (pool)" || p.UID == nil || *p.UID != 1000 || p.Connections == nil || *p.Connections != 1 {
		t.Fatalf("bad first sample: %+v", p)
	}
	counters(t, root, 1200, 30, 50, 300, 600)
	second, e := c.Sample(context.Background(), now.Add(2*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	p = second.Items[0]
	if p.CPUPercent == nil || *p.CPUPercent != 20 || p.ReadBytesPerSecond == nil || *p.ReadBytesPerSecond != 100 || *p.WriteBytesPerSecond != 200 {
		t.Fatalf("bad rates: %+v", p)
	}
	// PID reuse must reset every rate instead of comparing unrelated counters.
	counters(t, root, 1400, 200, 999, 3000, 6000)
	third, e := c.Sample(context.Background(), now.Add(4*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if third.Items[0].CPUPercent != nil || third.Items[0].ReadBytesPerSecond != nil {
		t.Fatal("PID reuse generated a rate")
	}
	if e := os.Remove(filepath.Join(root, "42", "io")); e != nil {
		t.Fatal(e)
	}
	fourth, e := c.Sample(context.Background(), now.Add(6*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if fourth.Items[0].ReadBytesPerSecond != nil {
		t.Fatal("missing IO must remain unavailable")
	}
	if e := os.Remove(filepath.Join(root, "42", "stat")); e != nil {
		t.Fatal(e)
	}
	last, e := c.Sample(context.Background(), now.Add(8*time.Second))
	if e != nil || len(last.Items) != 0 {
		t.Fatalf("exited process retained: %+v %v", last, e)
	}
}
func TestCounterResetsAndMalformedStat(t *testing.T) {
	root := fixture(t)
	c := NewCollector(root)
	now := time.Now()
	counters(t, root, 1000, 100, 50, 1000, 2000)
	_, _ = c.Sample(context.Background(), now)
	counters(t, root, 900, 1, 50, 1, 2)
	s, e := c.Sample(context.Background(), now.Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	p := s.Items[0]
	if p.CPUPercent != nil || p.ReadBytesPerSecond != nil {
		t.Fatal("reset generated invalid rates")
	}
	write(t, filepath.Join(root, "42", "stat"), "broken")
	s, e = c.Sample(context.Background(), now.Add(2*time.Second))
	if e != nil || len(s.Items) != 0 {
		t.Fatal("malformed process should be skipped")
	}
}
func TestFDLimitDoesNotInventZero(t *testing.T) {
	root := fixture(t)
	fd := filepath.Join(root, "42", "fd")
	write(t, filepath.Join(fd, "1"), "")
	write(t, filepath.Join(fd, "2"), "")
	budget := 1
	if countConnections(fd, &budget) != nil {
		t.Fatal("partial scan must be unavailable")
	}
}
