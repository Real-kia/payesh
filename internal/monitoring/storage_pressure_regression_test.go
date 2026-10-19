package monitoring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func seedPressureSamples(t *testing.T, s *Store, now time.Time, sequence uint64) {
	t.Helper()
	if err := s.UpsertServer(t.Context(), testServer()); err != nil {
		t.Fatal(err)
	}
	result, err := s.IngestNodeBatch(t.Context(), testServer().ID, []contracts.NodeMetricSample{
		overviewSample(sequence, now.Add(-30*time.Second), 10, "1000", "100"),
		overviewSample(sequence+1, now.Add(-15*time.Second), 20, "16000", "250"),
	}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range result.InsertedSamples {
		if err := s.AcknowledgePostProcessSample(t.Context(), sample); err != nil {
			t.Fatal(err)
		}
	}
}

// Repeated updates grow the journal while leaving live data nearly unchanged.
func growPressureWAL(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`PRAGMA wal_autocheckpoint=0; CREATE TABLE journal_churn(id INTEGER PRIMARY KEY, value BLOB); INSERT INTO journal_churn VALUES(1,zeroblob(32768))`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if _, err := s.db.Exec(`UPDATE journal_churn SET value=randomblob(32768) WHERE id=1`); err != nil {
			t.Fatal(err)
		}
	}
	s.maxBytes = 8 << 20
	usage, err := s.budgetUsage(false)
	if err != nil || usage < s.maxBytes {
		t.Fatalf("fixture must exceed its budget: usage=%d err=%v", usage, err)
	}
}

func TestStoragePressureReclaimsJournalBeforeDeletingSamples(t *testing.T) {
	s := openDiskStore(t)
	now := time.Now().UTC()
	seedPressureSamples(t, s, now, 0)
	growPressureWAL(t, s)
	if err := s.EnforceStorageLimit(t.Context()); err != nil {
		t.Fatal(err)
	}
	overview, err := s.QueryOverview(t.Context(), now, nil)
	if err != nil || len(overview.Items) != 1 || len(overview.Items[0].Series.T) != 2 || overview.Items[0].RX == nil || *overview.Items[0].RX != 1000 {
		t.Fatalf("journal pressure erased live monitoring: %+v, %v", overview, err)
	}
	usage, err := s.budgetUsage(false)
	if err != nil || usage >= s.maxBytes {
		t.Fatalf("journal not reclaimed: usage=%d err=%v", usage, err)
	}
}

func TestStoragePressurePinnedReaderDoesNotDeleteHistoryOrWaitForReader(t *testing.T) {
	s := openDiskStore(t)
	seedPressureSamples(t, s, time.Now().UTC().Add(-time.Hour), 0)
	reader, err := s.readDB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var count int
	if err := reader.QueryRow(`SELECT count(*) FROM metric_samples`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	growPressureWAL(t, s)
	if _, err := s.db.Exec(`PRAGMA busy_timeout=7777`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		err = s.EnforceStorageLimit(ctx)
		if !errors.Is(err, ErrStoragePressure) {
			t.Fatalf("pinned journal must apply prompt backpressure: %v", err)
		}
		if ctx.Err() != nil {
			t.Fatal("cleanup waited for the pinned reader")
		}
		// Authentication state still needs to be writable while history is
		// paused; these control writes must not queue behind a checkpoint.
		if err := s.SaveAuthState([]byte(`{}`)); err != nil {
			t.Fatalf("authentication persistence blocked: %v", err)
		}
	}
	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 7777 {
		t.Fatalf("checkpoint changed the writer timeout: %d, %v", timeout, err)
	}
	if err := s.readDB().QueryRow(`SELECT count(*) FROM metric_samples`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("cleanup destroyed history without reclaiming the pinned journal: count=%d err=%v", count, err)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnforceStorageLimit(t.Context()); err != nil {
		t.Fatalf("writes did not recover after the reader released its snapshot: %v", err)
	}
}

func TestStoragePressureEvictsOldRollupsAndKeepsLiveTraffic(t *testing.T) {
	s := openDiskStore(t)
	now := time.Now().UTC()
	seedPressureSamples(t, s, now.Add(-48*time.Hour), 0)
	seedPressureSamples(t, s, now, 2)
	for batch := 0; batch < 5; batch++ {
		rollups := make([]Rollup, 200)
		for i := range rollups {
			rollups[i] = Rollup{ServerID: testServer().ID, Metric: fmt.Sprintf("history.%d.%s", batch*200+i, strings.Repeat("x", 4096)), BucketStart: now.Add(-48 * time.Hour), BucketSeconds: 3600, SampleCount: 2, CounterDelta: "100", Coverage: "complete"}
		}
		if err := s.PutRollups(t.Context(), rollups); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := s.initStorageSettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Exercise persisted settings with a scaled budget, as in the existing
	// configured cleanup regression; public policy validation is unchanged.
	if _, err := s.db.Exec(`UPDATE storage_settings SET max_bytes=8388608`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnforceStorageLimit(t.Context()); err != nil {
		t.Fatal(err)
	}
	overview, err := s.QueryOverview(t.Context(), now, nil)
	if err != nil || len(overview.Items) != 1 || overview.Items[0].RX == nil || *overview.Items[0].RX != 1000 {
		t.Fatalf("cleanup removed live samples ahead of old summaries: %+v, %v", overview, err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM metric_rollups WHERE metric LIKE 'history.%'`).Scan(&remaining); err != nil || remaining >= 1000 {
		t.Fatalf("old summaries were not evicted: count=%d err=%v", remaining, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM metric_samples`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("old raw samples were not evicted: count=%d err=%v", remaining, err)
	}
	old := now.Add(-48 * time.Hour)
	result, err := s.IngestNodeBatch(t.Context(), testServer().ID, []contracts.NodeMetricSample{
		overviewSample(0, old.Add(-30*time.Second), 10, "1000", "100"),
		overviewSample(1, old.Add(-15*time.Second), 20, "16000", "250"),
	}, nil, now)
	if err != nil || result.Inserted != 0 {
		t.Fatalf("evicted history was resurrected by replay: %+v, %v", result, err)
	}
}
