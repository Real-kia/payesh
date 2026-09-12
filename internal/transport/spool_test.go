package transport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestSpoolAppendAckAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent", "offline.spool")
	spool, err := OpenSpool(path, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	first := spoolSample(0, "epoch-spool-01")
	if gaps, err := spool.Append(first); err != nil || len(gaps) != 0 {
		t.Fatalf("append first: gaps=%v err=%v", gaps, err)
	}
	second := spoolSample(1, "epoch-spool-01")
	if _, err := spool.Append(second); err != nil {
		t.Fatal(err)
	}
	if err := spool.Acknowledge("epoch-spool-01", 0); err != nil {
		t.Fatal("acknowledge:", err)
	}
	reopened, err := OpenSpool(path, 8<<10)
	if err != nil {
		t.Fatal("reopen:", err)
	}
	pending, err := reopened.Pending(0)
	if err != nil {
		t.Fatal("pending:", err)
	}
	if len(pending) != 1 || len(pending[0].Batch.Samples) != 1 || pending[0].Batch.Samples[0].Sequence != 1 {
		t.Fatalf("unexpected pending after restart: %+v", pending)
	}
	if mode := fileMode(t, path); mode.Perm() != 0o600 {
		t.Fatalf("spool mode=%o, want 600", mode.Perm())
	}
}

func TestSpoolEvictionReportsExplicitGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offline.spool")
	spool, err := OpenSpool(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(0); sequence < 4; sequence++ {
		gaps, err := spool.Append(spoolSample(sequence, "epoch-spool-02"))
		if err != nil {
			t.Fatal("append:", err)
		}
		if sequence == 0 && len(gaps) != 0 {
			t.Fatal("unexpected gap on first append")
		}
	}
	pending, err := spool.Pending(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 || len(pending) >= 4 {
		t.Fatalf("eviction did not bound records: %d", len(pending))
	}
	// The queue is small enough that at least one later append must evict an
	// older sample. Re-append until the explicit gap is observed.
	var gaps []contracts.CoverageGap
	for sequence := uint64(4); sequence < 8 && len(gaps) == 0; sequence++ {
		gaps, err = spool.Append(spoolSample(sequence, "epoch-spool-02"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(gaps) == 0 || gaps[0].Reason != "spool-eviction" || gaps[0].CollectorEpoch != "epoch-spool-02" {
		t.Fatalf("missing eviction gap: %+v", gaps)
	}
}

func TestSpoolEvictionGapIsDurableAndPrecedesSamplesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offline.spool")
	spool, err := OpenSpool(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	observedEviction := false
	for sequence := uint64(0); sequence < 12; sequence++ {
		gaps, appendErr := spool.Append(spoolSample(sequence, "epoch-spool-restart"))
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		if len(gaps) > 0 {
			observedEviction = true
			break
		}
	}
	if !observedEviction {
		t.Fatal("test did not force a spool eviction")
	}
	reopened, err := OpenSpool(path, 512)
	if err != nil {
		t.Fatal("reopen:", err)
	}
	pending, err := reopened.Pending(0)
	if err != nil {
		t.Fatal("pending:", err)
	}
	gapIndex, sampleIndex := -1, -1
	for index, record := range pending {
		if len(record.Batch.Gaps) > 0 && gapIndex == -1 {
			gapIndex = index
		}
		if len(record.Batch.Samples) > 0 && sampleIndex == -1 {
			sampleIndex = index
		}
	}
	if gapIndex == -1 || sampleIndex == -1 || gapIndex > sampleIndex {
		t.Fatalf("durable eviction gap did not precede samples: gap=%d sample=%d pending=%+v", gapIndex, sampleIndex, pending)
	}
}

func TestSpoolMultiEpochEvictionPersistsSeparateGapRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi-epoch.spool")
	spool, err := OpenSpool(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	seenEpochs := make(map[contracts.CollectorEpoch]bool)
	for sequence := uint64(0); sequence < 16 && len(seenEpochs) < 2; sequence++ {
		for _, epoch := range []contracts.CollectorEpoch{"epoch-multi-a", "epoch-multi-b"} {
			gaps, appendErr := spool.Append(spoolSample(sequence, epoch))
			if appendErr != nil {
				t.Fatal(appendErr)
			}
			for _, gap := range gaps {
				seenEpochs[gap.CollectorEpoch] = true
			}
		}
	}
	if len(seenEpochs) != 2 {
		t.Fatalf("did not force eviction across both epochs: %v", seenEpochs)
	}
	reopened, err := OpenSpool(path, 512)
	if err != nil {
		t.Fatal("reopen:", err)
	}
	pending, err := reopened.Pending(0)
	if err != nil {
		t.Fatal("pending:", err)
	}
	firstSampleByEpoch := make(map[contracts.CollectorEpoch]int)
	firstGapByEpoch := make(map[contracts.CollectorEpoch]int)
	for index, record := range pending {
		for _, gap := range record.Batch.Gaps {
			if firstGapByEpoch[gap.CollectorEpoch] == 0 {
				firstGapByEpoch[gap.CollectorEpoch] = index + 1
			}
		}
		for _, sample := range record.Batch.Samples {
			if firstSampleByEpoch[sample.CollectorEpoch] == 0 {
				firstSampleByEpoch[sample.CollectorEpoch] = index + 1
			}
		}
		if len(record.Batch.Samples) > 0 && len(record.Batch.Gaps) > 0 {
			t.Fatalf("mixed-epoch or mixed-purpose durable record: %+v", record.Batch)
		}
	}
	for epoch := range seenEpochs {
		if firstGapByEpoch[epoch] == 0 {
			t.Fatalf("missing durable gap for %s: pending=%+v", epoch, pending)
		}
		if firstSampleByEpoch[epoch] != 0 && firstGapByEpoch[epoch] > firstSampleByEpoch[epoch] {
			t.Fatalf("gap ordering for %s: gap=%d sample=%d pending=%+v", epoch, firstGapByEpoch[epoch], firstSampleByEpoch[epoch], pending)
		}
	}
}

func TestSpoolRejectsCorruptionAndOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offline.spool")
	if err := os.WriteFile(path, []byte{0, 0, 0, 10, '{'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpool(path, 1024); !errors.Is(err, ErrSpoolCorrupt) {
		t.Fatalf("open corrupt err=%v", err)
	}
	spool, err := OpenSpool(filepath.Join(t.TempDir(), "offline.spool"), 512)
	if err != nil {
		t.Fatal(err)
	}
	sample := spoolSample(0, "epoch-spool-03")
	sample.Samples[0].Values["large"] = 1
	for i := 0; i < 240; i++ {
		sample.Samples[0].Values[fmt.Sprintf("metric_%03d_%s", i, strings.Repeat("x", 100))] = float64(i)
	}
	if _, err := spool.Append(sample); !errors.Is(err, ErrSpoolRecordTooBig) {
		t.Fatalf("oversized append err=%v", err)
	}
}

func spoolSample(sequence uint64, epoch contracts.CollectorEpoch) contracts.SampleBatch {
	return contracts.SampleBatch{Samples: []contracts.NodeMetricSample{{
		ServerID: "server-spool-test01", CollectorEpoch: epoch, Sequence: sequence,
		ObservedAt: time.Unix(int64(sequence), 0).UTC(), Values: map[string]float64{"cpu.total": 1},
	}}}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}
