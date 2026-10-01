package monitoring

import (
	"context"
	"errors"
	"github.com/Real-kia/payesh/internal/contracts"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorageSettingsPersistAcrossProcessesAndPressure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "monitor.db")
	s, err := OpenStore(ctx, path, StoreOptions{MaxBytes: DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, ok, err := s.StorageSettings(ctx)
	if err != nil || !ok || p.MaxDatabaseBytes != 1000000000 || p.Revision != 1 {
		t.Fatalf("default=%+v %v", p, err)
	}
	p.MaxDatabaseBytes = 200000000
	p.SampleSeconds = 10
	p.PressureSampleSeconds = 60
	if err := s.SaveStorageSettings(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStorageSettings(ctx, p); !errors.Is(err, ErrSettingsConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	other, err := OpenStore(ctx, path, StoreOptions{MaxBytes: DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	p, _, _ = other.StorageSettings(ctx)
	if p.MaxDatabaseBytes != 200000000 || p.SampleSeconds != 10 {
		t.Fatalf("settings reset on reopen: %+v", p)
	}
	if v := ReadLocalSamplingSeconds(ctx, path); v != 10 {
		t.Fatalf("local agent interval=%d", v)
	}
	if err := s.updateStoragePressure(ctx, 170000000); err != nil {
		t.Fatal(err)
	}
	if err := s.updateStoragePressure(ctx, 190000000); err != nil {
		t.Fatal(err)
	}
	if v := other.SamplingSeconds(ctx); v != 60 {
		t.Fatalf("pressure did not slow other process: %d", v)
	}
	if v := ReadLocalSamplingSeconds(ctx, path); v != 60 {
		t.Fatalf("pressure did not reach local agent: %d", v)
	}
	if err := s.updateStoragePressure(ctx, 160000000); err != nil {
		t.Fatal(err)
	}
	if v := other.SamplingSeconds(ctx); v != 60 {
		t.Fatalf("hysteresis oscillated: %d", v)
	}
	if err := s.notifyStorageCleanup(ctx, 200, 190000000, 160000000); err != nil {
		t.Fatal(err)
	}
	if err := s.updateStoragePressure(ctx, 140000000); err != nil {
		t.Fatal(err)
	}
	if v := other.SamplingSeconds(ctx); v != 10 {
		t.Fatalf("normal sampling did not recover: %d", v)
	}
	notes, err := s.StorageNotifications(ctx)
	if err != nil || len(notes) != 4 {
		t.Fatalf("notifications=%+v err=%v", notes, err)
	}
	if err := s.MarkStorageNotificationsRead(ctx); err != nil {
		t.Fatal(err)
	}
	notes, _ = other.StorageNotifications(ctx)
	for _, n := range notes {
		if !n.Read {
			t.Fatal("read state not persisted")
		}
	}
	p, _, _ = s.StorageSettings(ctx)
	p.NotificationsEnabled = false
	p.AdaptiveSampling = false
	if err := s.SaveStorageSettings(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.updateStoragePressure(ctx, 195000000); err != nil {
		t.Fatal(err)
	}
	if other.SamplingSeconds(ctx) != 10 {
		t.Fatal("disabled adaptation still slows sampling")
	}
	notes, _ = s.StorageNotifications(ctx)
	if len(notes) != 4 {
		t.Fatal("disabled notifications still emit")
	}
}
func TestStorageNotificationsAreBounded(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 120; i++ {
		if err := insertStorageNotificationTx(ctx, tx, "storage_cleanup", "test cleanup", time.Unix(int64(i*600), 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	notes, err := s.StorageNotifications(ctx)
	if err != nil || len(notes) != 100 {
		t.Fatalf("bounded notes=%d err=%v", len(notes), err)
	}
}
func TestStorageSettingsRejectUnsafeBounds(t *testing.T) {
	for _, edit := range []func(*StorageSettings){func(p *StorageSettings) { p.MaxDatabaseBytes = 1 }, func(p *StorageSettings) { p.MaxDatabaseBytes = 65000000000 }, func(p *StorageSettings) { p.SampleSeconds = 1 }, func(p *StorageSettings) { p.PressureSampleSeconds = 5 }} {
		p := DefaultStorageSettings()
		edit(&p)
		if p.Validate() == nil {
			t.Fatalf("invalid policy accepted %+v", p)
		}
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "absent.db")
	if ReadLocalSamplingSeconds(ctx, path) != 0 {
		t.Fatal("absent policy did not fall back")
	}
}

func TestConfiguredDatabaseCleanupPreservesIdentityAndBilling(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cleanup.db")
	s, err := OpenStore(ctx, path, StoreOptions{MaxBytes: DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := testServer()
	if err := s.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	period := contracts.TrafficPeriod{Scope: "host", From: base, To: base.Add(31 * 24 * time.Hour), Timezone: "UTC", AllowanceBytes: 100000, Direction: "combined", CountedBytes: 12345, Continuity: "complete"}
	if err := s.UpsertTrafficPeriod(ctx, server.ID, period); err != nil {
		t.Fatal(err)
	}
	for batch := 0; batch < 5; batch++ {
		entries := make([]Rollup, 200)
		for i := range entries {
			entries[i] = Rollup{ServerID: server.ID, Metric: "test." + strings.Repeat("x", 4096), BucketStart: base.Add(time.Duration(batch*200+i) * time.Hour), BucketSeconds: 3600, SampleCount: 2, CounterDelta: "100", Coverage: "complete"}
		}
		if err := s.PutRollups(ctx, entries); err != nil {
			t.Fatal(err)
		}
	}
	lateOld := Rollup{ServerID: server.ID, Metric: "test.late-old", BucketStart: base.Add(-time.Hour), BucketSeconds: 3600, SampleCount: 2, CounterDelta: "1", Coverage: "complete"}
	if err := s.PutRollups(ctx, []Rollup{lateOld}); err != nil {
		t.Fatal(err)
	}
	// A reduced fixture budget exercises the same cleanup path without creating
	// hundreds of megabytes of test data. Public settings enforce larger bounds.
	if _, err := s.db.ExecContext(ctx, `UPDATE storage_settings SET max_bytes=8388608 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	before, err := s.DatabaseBytes()
	if err != nil {
		t.Fatal(err)
	}
	if before <= 8388608 {
		t.Fatalf("fixture not under pressure: %d", before)
	}
	_ = s.EnforceStorageLimit(ctx)
	after, err := s.DatabaseBytes()
	if err != nil {
		t.Fatal(err)
	}
	if after >= before {
		t.Fatalf("cleanup did not reclaim database pages: %d -> %d", before, after)
	}
	var oldRows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM metric_rollups WHERE metric='test.late-old'`).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatal("late-arriving old history was retained ahead of newer rows", err)
	}
	if _, ok, err := s.GetServer(ctx, server.ID); err != nil || !ok {
		t.Fatal("cleanup removed server identity")
	}
	page, err := s.QueryTrafficPeriods(ctx, server.ID, base, base.Add(time.Hour), "host", 10, "")
	if err != nil || len(page.Periods) != 1 || page.Periods[0].CountedBytes != 12345 {
		t.Fatalf("cleanup removed billing totals: %+v %v", page, err)
	}
	notes, err := s.StorageNotifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range notes {
		if note.Kind == "storage_cleanup" {
			found = true
		}
	}
	if !found {
		t.Fatal("cleanup was not reported")
	}
}

func TestFullDatabaseNotificationIsDeduplicated(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, ":memory:", StoreOptions{MaxBytes: DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if err := s.notifyStorageLimitReached(ctx); err != nil {
			t.Fatal(err)
		}
	}
	notes, err := s.StorageNotifications(ctx)
	if err != nil || len(notes) != 1 || notes[0].Kind != "storage_full" || notes[0].Read {
		t.Fatalf("full notification=%+v %v", notes, err)
	}
}
