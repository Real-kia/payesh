package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateConfigBounds(t *testing.T) {
	base := config{duration: time.Second, interval: 10 * time.Millisecond, pruneInterval: time.Second, retentionAge: time.Second, batchSize: 1}
	for name, cfg := range map[string]config{
		"zero duration":       func() config { c := base; c.duration = 0; return c }(),
		"duration too long":   func() config { c := base; c.duration = maxDuration + time.Nanosecond; return c }(),
		"zero interval":       func() config { c := base; c.interval = 0; return c }(),
		"zero prune interval": func() config { c := base; c.pruneInterval = 0; return c }(),
		"zero retention":      func() config { c := base; c.retentionAge = 0; return c }(),
		"batch too large":     func() config { c := base; c.batchSize = maxBatchSize + 1; return c }(),
		"negative bytes":      func() config { c := base; c.maxBytes = -1; return c }(),
		"memory database":     func() config { c := base; c.dbPath = ":memory:"; return c }(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateConfig(cfg); err == nil {
				t.Fatal("expected invalid configuration")
			}
		})
	}
}

func TestRunShortSoakObservesPruneAndCleansTemporaryState(t *testing.T) {
	cfg := config{duration: 450 * time.Millisecond, interval: 40 * time.Millisecond, pruneInterval: 80 * time.Millisecond, retentionAge: 100 * time.Millisecond, batchSize: 2, maxBytes: 64 << 20, quiet: true}
	var progress, diagnostics bytes.Buffer
	r, err := run(context.Background(), cfg, &progress, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if !r.CleanTeardown {
		t.Fatal("temporary soak state was not cleaned up")
	}
	if r.AttemptedSamples == 0 || r.InsertedSamples == 0 {
		t.Fatalf("no samples ingested: %+v", r)
	}
	if r.IngestErrors != 0 || r.PruneErrors != 0 {
		t.Fatalf("unexpected errors: %+v diagnostics=%s", r, diagnostics.String())
	}
	if r.PruneRuns == 0 || !r.RetentionObserved || r.PruneDeleted == 0 {
		t.Fatalf("retention was not observed: %+v", r)
	}
	if r.StorageBytesPeak == 0 || r.WALBytesPeak == 0 {
		t.Fatalf("storage/WAL metrics missing: %+v", r)
	}
	if progress.Len() != 0 {
		t.Fatalf("quiet run emitted progress: %s", progress.String())
	}
}

func TestRunCanKeepExplicitDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nested", "soak.db")
	cfg := config{duration: 120 * time.Millisecond, interval: 40 * time.Millisecond, pruneInterval: time.Second, retentionAge: time.Second, batchSize: 1, maxBytes: 64 << 20, dbPath: dbPath, keep: true, quiet: true}
	var progress, diagnostics bytes.Buffer
	r, err := run(context.Background(), cfg, &progress, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if r.CleanTeardown {
		t.Fatal("explicit kept database reported clean temporary teardown")
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("kept database missing: %v", err)
	}
	if !strings.Contains(progress.String(), "database retained at") {
		t.Fatalf("retained path was not reported: %s", progress.String())
	}
}
