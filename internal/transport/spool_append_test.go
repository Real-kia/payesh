package transport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// realisticBatch is about the size of a real node sample (~3 KB of JSON).
func realisticBatch(sequence uint64) contracts.SampleBatch {
	values := make(map[string]float64, 60)
	for i := 0; i < 60; i++ {
		values[fmt.Sprintf("metric.name.%02d", i)] = float64(i) * 1.5
	}
	return contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{ServerID: "server-spool-load-01", CollectorEpoch: "epoch-load", Sequence: sequence, ObservedAt: time.Unix(int64(sequence)*15, 0).UTC(), Values: values}}}
}

// A full spool must stay cheap: an outage of days must not turn every new
// sample into a rewrite of the whole file, and catching up must not either.
func TestSpoolStaysCheapWhenFullAndWhileCatchingUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.spool")
	spool, err := OpenSpool(path, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var sequence uint64
	evictions := 0
	for ; sequence < 3000; sequence++ { // ~4.5 MB of samples into a 4 MB spool
		gaps, err := spool.Append(realisticBatch(sequence))
		if err != nil {
			t.Fatal(err)
		}
		if len(gaps) > 0 {
			evictions++
		}
	}
	fill := time.Since(start)
	// Each append is one small write plus fsync (a few ms on macOS, where
	// Sync is a full flush). The old rewrite-everything spool needed ~76 ms
	// per append at 1 MB and grew with the file; this bound catches that.
	t.Logf("3000 appends into a full spool: %v (%v each), %d evictions", fill, fill/3000, evictions)
	if fill/3000 > 15*time.Millisecond {
		t.Fatalf("appends average %v", fill/3000)
	}
	if evictions == 0 || evictions > 40 {
		t.Fatalf("evictions = %d; want a few batched evictions, not one per sample", evictions)
	}
	if info, _ := os.Stat(path); info.Size() > 4<<20 {
		t.Fatalf("spool grew past its bound: %d", info.Size())
	}
	// The raw-copied file must reopen and validate, gap record first.
	reopened, err := OpenSpool(path, 4<<20)
	if err != nil {
		t.Fatalf("reopen after eviction: %v", err)
	}
	if first, _ := reopened.Pending(1); len(first) != 1 || len(first[0].Batch.Gaps) == 0 {
		t.Fatalf("first record after eviction is not the gap: %+v", first)
	}

	// Catch up the way the agent replays: one record, then its ack.
	start = time.Now()
	sent := 0
	for {
		pending, err := spool.Pending(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		batch := pending[0].Batch
		through := uint64(0)
		for _, sample := range batch.Samples {
			through = max(through, sample.Sequence)
		}
		for _, gap := range batch.Gaps {
			through = max(through, gap.ToSequence)
		}
		if err := spool.Acknowledge(batchEpoch(batch), through); err != nil {
			t.Fatal(err)
		}
		sent++
	}
	catchUp := time.Since(start)
	t.Logf("catching up %d records: %v (%v each)", sent, catchUp, catchUp/time.Duration(max(sent, 1)))
	if catchUp/time.Duration(max(sent, 1)) > 15*time.Millisecond {
		t.Fatalf("catching up %d records took %v", sent, catchUp)
	}
	if info, _ := os.Stat(path); info.Size() != 0 {
		t.Fatalf("spool not empty after catching up: %d bytes", info.Size())
	}
}

func TestSpoolHeadSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.spool")
	spool, err := OpenSpool(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(0); sequence < 10; sequence++ {
		if _, err := spool.Append(spoolSample(sequence, "epoch-head")); err != nil {
			t.Fatal(err)
		}
	}
	if err := spool.Acknowledge("epoch-head", 2); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSpool(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 7 || pending[0].Batch.Samples[0].Sequence != 3 {
		t.Fatalf("pending after restart = %d records starting %+v", len(pending), pending[0].Batch.Samples)
	}
	// A lost head file only resends already-accepted records; it never loses any.
	if err := os.Remove(path + ".head"); err != nil {
		t.Fatal(err)
	}
	again, _ := OpenSpool(path, 1<<20)
	if all, _ := again.Pending(0); len(all) < 7 {
		t.Fatalf("records lost without the head file: %d", len(all))
	}
}

func TestSpoolRepairsTornAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.spool")
	spool, err := OpenSpool(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(0); sequence < 3; sequence++ {
		if _, err := spool.Append(spoolSample(sequence, "epoch-torn")); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a crash in the middle of writing a fourth record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{0, 0, 1, 0, '{', '"', 'b'})
	_ = f.Close()
	reopened, err := OpenSpool(path, 1<<20)
	if err != nil {
		t.Fatalf("torn append made the spool unreadable: %v", err)
	}
	pending, _ := reopened.Pending(0)
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want the 3 complete records", len(pending))
	}
	if _, err := reopened.Append(spoolSample(3, "epoch-torn")); err != nil {
		t.Fatal(err)
	}
	if final, _ := OpenSpool(path, 1<<20); final != nil {
		if all, _ := final.Pending(0); len(all) != 4 {
			t.Fatalf("after repair and append: %d records", len(all))
		}
	}
}

func TestSpoolPartialAcknowledgementKeepsSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.spool")
	spool, err := OpenSpool(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	batch := spoolSample(0, "epoch-partial")
	second := spoolSample(1, "epoch-partial").Samples[0]
	third := spoolSample(2, "epoch-partial").Samples[0]
	batch.Samples = append(batch.Samples, second, third)
	if _, err := spool.Append(batch); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Append(spoolSample(5, "epoch-other")); err != nil {
		t.Fatal(err)
	}
	if err := spool.Acknowledge("epoch-partial", 1); err != nil {
		t.Fatal(err)
	}
	pending, _ := spool.Pending(0)
	if len(pending) != 2 || len(pending[0].Batch.Samples) != 1 || pending[0].Batch.Samples[0].Sequence != 2 || pending[1].Batch.Samples[0].CollectorEpoch != "epoch-other" {
		t.Fatalf("partial ack result: %+v", pending)
	}
	// An acknowledgement for another epoch never removes this epoch's records.
	if err := spool.Acknowledge("epoch-unknown", 100); err != nil {
		t.Fatal(err)
	}
	if again, _ := spool.Pending(0); len(again) != 2 {
		t.Fatalf("unknown epoch removed records: %d", len(again))
	}
}

// The memory an agent needs to evict from a full 32 MB spool.
func TestSpoolEvictionMemoryAtFullSize(t *testing.T) {
	if os.Getenv("PAYESH_SPOOL_LOAD_TEST") != "1" {
		t.Skip("set PAYESH_SPOOL_LOAD_TEST=1 to fill a full 32 MB spool (slow: one fsync per sample)")
	}
	path := filepath.Join(t.TempDir(), "agent.spool")
	spool, err := OpenSpool(path, MaxSpoolBytes)
	if err != nil {
		t.Fatal(err)
	}
	// Build the file directly (fast) and then open it like an agent restart.
	var sequence uint64
	for spool.liveSize() < MaxSpoolBytes-64<<10 {
		if _, err := spool.Append(realisticBatch(sequence)); err != nil {
			t.Fatal(err)
		}
		sequence++
		if sequence > 40000 {
			break
		}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	evicted := false
	for i := 0; i < 200 && !evicted; i++ {
		gaps, err := spool.Append(realisticBatch(sequence))
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		evicted = len(gaps) > 0
	}
	runtime.ReadMemStats(&after)
	if !evicted {
		t.Fatal("no eviction happened")
	}
	allocated := (after.TotalAlloc - before.TotalAlloc) >> 20
	t.Logf("full %d MB spool, %d samples: eviction allocated %d MB in total", MaxSpoolBytes>>20, sequence, allocated)
	if allocated > 64 {
		t.Fatalf("eviction allocated %d MB", allocated)
	}
}

// legacyRead is how agents before the append-only spool read the file: from
// the start, every record must decode and the total must fit the bound.
// Keeping the file readable this way lets an update roll back safely.
func legacyRead(t *testing.T, path string, maxSize int64) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > maxSize {
		t.Fatalf("spool file is %d bytes, over the %d bound older agents enforce", len(data), maxSize)
	}
	count := 0
	for offset := 0; offset < len(data); count++ {
		if len(data)-offset < 4 {
			t.Fatalf("trailing bytes at %d", offset)
		}
		length := int(uint32(data[offset])<<24 | uint32(data[offset+1])<<16 | uint32(data[offset+2])<<8 | uint32(data[offset+3]))
		if length == 0 || offset+4+length > len(data) {
			t.Fatalf("bad record at %d", offset)
		}
		var record storedSpoolRecord
		if err := json.Unmarshal(data[offset+4:offset+4+length], &record); err != nil || validateSpoolBatch(record.Batch) != nil {
			t.Fatalf("record at %d does not decode: %v", offset, err)
		}
		offset += 4 + length
	}
	return count
}

func TestSpoolStaysReadableByOlderAgents(t *testing.T) {
	const bound = 256 << 10
	path := filepath.Join(t.TempDir(), "agent.spool")
	spool, err := OpenSpool(path, bound)
	if err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	for round := 0; round < 6; round++ {
		// Fill past the bound, then acknowledge part of it (head advances).
		for i := 0; i < 250; i++ {
			if _, err := spool.Append(realisticBatch(sequence)); err != nil {
				t.Fatal(err)
			}
			sequence++
			legacyRead(t, path, bound)
		}
		pending, _ := spool.Pending(0)
		if len(pending) > 10 {
			last := pending[len(pending)/3].Batch.Samples
			if len(last) > 0 {
				if err := spool.Acknowledge("epoch-load", last[0].Sequence); err != nil {
					t.Fatal(err)
				}
			}
		}
		legacyRead(t, path, bound)
	}
}
