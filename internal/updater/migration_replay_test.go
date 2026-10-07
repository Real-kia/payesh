package updater

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func migrationStores(t *testing.T) (*monitoring.Store, *sql.DB, *monitoring.Store, *sql.DB, contracts.Server) {
	t.Helper()
	ctx := context.Background()
	sourcePath, destPath := t.TempDir()+"/source.db", t.TempDir()+"/dest.db"
	source, err := monitoring.OpenStore(ctx, sourcePath, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	dest, err := monitoring.OpenStore(ctx, destPath, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dest.Close() })
	server := contracts.Server{ID: "server-0123456789", Name: "source", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}
	if err := source.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	return source, openDB(t, sourcePath), dest, openDB(t, destPath), server
}

func execMigrationSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationTailReconcilesMutableState(t *testing.T) {
	_, source, _, dest, server := migrationStores(t)
	at := time.Now().UTC().Truncate(time.Second)
	start, end := formatMigrationTime(at), formatMigrationTime(at.Add(time.Hour))
	execMigrationSQL(t, source, `INSERT INTO control_policies VALUES(?,?,?,?,?,?,?,?,?,NULL)`, string(server.ID), "bandwidth", "interface", "eth0", "limit", "applied", "{}", 1, start)
	execMigrationSQL(t, source, `INSERT INTO traffic_periods VALUES(?,?,?,?,?,?,?,?,?,?)`, string(server.ID), "host", start, end, "UTC", "1000", "combined", "10", "complete", "[]")
	execMigrationSQL(t, source, `INSERT INTO metric_rollups VALUES(?,?,?,?,?,?,?,?,?,?,?)`, string(server.ID), "cpu.utilization", start, 60, 1, 1, .1, .1, .1, "", "complete")
	ctx := context.Background()
	initial, err := ExportMigration(ctx, source, ExportOptions{ServerID: server.ID, ReplaySafe: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportMigration(ctx, dest, initial, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, source, `UPDATE control_policies SET revision=2,parameters_json='{"limit":20}'`)
	execMigrationSQL(t, source, `UPDATE traffic_periods SET counted_bytes='25'`)
	execMigrationSQL(t, source, `UPDATE metric_rollups SET sample_count=2,weighted_mean=.3`)
	tail, err := ExportMigration(ctx, source, ExportOptions{ServerID: server.ID, ReplaySafe: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportMigration(ctx, dest, tail, ImportOptions{PreviousArtifact: initial}); err != nil {
		t.Fatal(err)
	}
	var revision, count int
	var bytes string
	if err := dest.QueryRow(`SELECT revision FROM control_policies WHERE server_id=?`, server.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := dest.QueryRow(`SELECT counted_bytes FROM traffic_periods WHERE server_id=?`, server.ID).Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	if err := dest.QueryRow(`SELECT sample_count FROM metric_rollups WHERE server_id=?`, server.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || bytes != "25" || count != 2 {
		t.Fatalf("stale final state: policy=%d traffic=%s rollup=%d", revision, bytes, count)
	}
}

func TestMigrationPreservesRetainedFrontierAndRejectsReplay(t *testing.T) {
	_, source, destStore, dest, server := migrationStores(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	execMigrationSQL(t, source, `INSERT INTO sequence_frontiers VALUES(?,?,?,?)`, server.ID, "epoch-live", "8", 0)
	execMigrationSQL(t, source, `INSERT INTO collector_epoch_metadata VALUES(?,?,?,?)`, server.ID, "epoch-live", formatMigrationTime(at), formatMigrationTime(at))
	execMigrationSQL(t, source, `INSERT INTO metric_sample_tombstones VALUES(?,?,?,?)`, server.ID, "epoch-live", "0", "7")
	execMigrationSQL(t, source, `INSERT INTO collector_epoch_retirements VALUES(?,?,?)`, server.ID, "epoch-retired", formatMigrationTime(at))
	artifact, err := ExportMigration(ctx, source, ExportOptions{ServerID: server.ID, ReplaySafe: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportMigration(ctx, dest, artifact, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, err := destStore.SequenceFrontierReady(ctx, server.ID, "epoch-live", 7)
	if err != nil || !ready {
		t.Errorf("retained acknowledgement frontier missing: ready=%v err=%v", ready, err)
	}
	for _, epoch := range []contracts.CollectorEpoch{"epoch-live", "epoch-retired"} {
		sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: epoch, Sequence: 0, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": .2}}
		result, err := destStore.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, nil)
		if err == nil && result.Inserted != 0 {
			t.Errorf("retained-out payload resurrected: epoch=%s result=%+v", epoch, result)
		}
	}
}

func replayArtifact(t *testing.T, db *sql.DB, server contracts.ServerID) []byte {
	t.Helper()
	artifact, err := ExportMigration(context.Background(), db, ExportOptions{ServerID: server, ReplaySafe: true})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func TestReplayMigrationTailPreservesUnrelatedServerAndDeletions(t *testing.T) {
	sourceStore, source, destStore, dest, server := migrationStores(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	other := server
	other.ID = "unrelated-0123456789"
	other.Name = "other"
	if err := destStore.EnsureServer(ctx, other); err != nil {
		t.Fatal(err)
	}
	sample := contracts.MetricSample{ServerID: other.ID, CollectorEpoch: "other-epoch", Sequence: 0, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": .1}}
	if _, err := destStore.IngestSamples(ctx, other.ID, []contracts.MetricSample{sample}, nil); err != nil {
		t.Fatal(err)
	}
	untouched := replayArtifact(t, dest, other.ID)
	sample.ServerID = server.ID
	sample.CollectorEpoch = "source-epoch"
	if _, err := sourceStore.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, []contracts.CoverageGap{{CollectorEpoch: sample.CollectorEpoch, FromSequence: 1, ToSequence: 3, Reason: "transport-disconnect"}}); err != nil {
		t.Fatal(err)
	}
	first := replayArtifact(t, source, server.ID)
	if _, err := ImportMigration(ctx, dest, first, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	// Retention may delete a raw payload only after permanent replay authority.
	execMigrationSQL(t, source, `DELETE FROM metric_samples WHERE server_id=?`, server.ID)
	execMigrationSQL(t, source, `DELETE FROM post_process_queue WHERE server_id=?`, server.ID)
	execMigrationSQL(t, source, `INSERT INTO metric_sample_tombstones VALUES(?,?,?,?)`, server.ID, "source-epoch", "0", "0")
	second := replayArtifact(t, source, server.ID)
	if _, err := ImportMigration(ctx, dest, second, ImportOptions{PreviousArtifact: first}); err != nil {
		t.Fatal(err)
	}
	before, err := unwrapMigrationPayload(untouched, "")
	if err != nil {
		t.Fatal(err)
	}
	after, err := unwrapMigrationPayload(replayArtifact(t, dest, other.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if !sameReplaySnapshot(before, after) {
		t.Fatal("tail changed unrelated server")
	}
	result, err := destStore.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, nil)
	if err != nil || result.Inserted != 0 {
		t.Fatalf("replay after tail resurrected data: %+v err=%v", result, err)
	}
	var count int
	if err := dest.QueryRow(`SELECT COUNT(*) FROM metric_samples WHERE server_id=?`, server.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained-out sample survived: count=%d err=%v", count, err)
	}
}

func TestReplayMigrationRejectsDestinationDivergenceAndImmutableConflicts(t *testing.T) {
	for _, kind := range []string{"destination", "sample", "history", "server", "unproved-delete", "policy"} {
		t.Run(kind, func(t *testing.T) {
			sourceStore, source, _, dest, server := migrationStores(t)
			ctx := context.Background()
			at := time.Now().UTC().Truncate(time.Second)
			stamp := formatMigrationTime(at)
			sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "source-epoch", Sequence: 0, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": .1}}
			if _, err := sourceStore.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, nil); err != nil {
				t.Fatal(err)
			}
			execMigrationSQL(t, source, `INSERT INTO traffic_allowance_versions VALUES(?,?,?,?,?,?,?,?,?)`, server.ID, "host", "combined", stamp, "[]", "1000", 1, "UTC", "[]")
			execMigrationSQL(t, source, `INSERT INTO control_policies VALUES(?,?,?,?,?,?,?,?,?,NULL)`, server.ID, "bandwidth", "interface", "eth0", "limit", "applied", "{}", 1, stamp)
			first := replayArtifact(t, source, server.ID)
			if _, err := ImportMigration(ctx, dest, first, ImportOptions{}); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "destination":
				execMigrationSQL(t, dest, `UPDATE servers SET name='edited-locally' WHERE id=?`, server.ID)
			case "sample":
				execMigrationSQL(t, source, `UPDATE metric_samples SET values_json='{"cpu.utilization":0.9}' WHERE server_id=?`, server.ID)
			case "history":
				execMigrationSQL(t, source, `UPDATE traffic_allowance_versions SET allowance_bytes='999' WHERE server_id=?`, server.ID)
			case "server":
				execMigrationSQL(t, source, `UPDATE servers SET architecture='arm64' WHERE id=?`, server.ID)
			case "unproved-delete":
				execMigrationSQL(t, source, `DELETE FROM metric_samples WHERE server_id=?`, server.ID)
				execMigrationSQL(t, source, `DELETE FROM post_process_queue WHERE server_id=?`, server.ID)
				execMigrationSQL(t, source, `DELETE FROM sequence_frontiers WHERE server_id=?`, server.ID)
			case "policy":
				execMigrationSQL(t, source, `UPDATE control_policies SET parameters_json='{"limit":99}' WHERE server_id=?`, server.ID)
			}
			before := replayArtifact(t, dest, server.ID)
			tail := replayArtifact(t, source, server.ID)
			if _, err := ImportMigration(ctx, dest, tail, ImportOptions{PreviousArtifact: first}); err == nil {
				t.Fatalf("%s conflict was silently reconciled", kind)
			}
			a, _ := unwrapMigrationPayload(before, "")
			b, _ := unwrapMigrationPayload(replayArtifact(t, dest, server.ID), "")
			if !sameReplaySnapshot(a, b) {
				t.Fatal("failed import partially changed destination")
			}
		})
	}
}

func TestReplayMigrationTransfersUsageAndPendingWork(t *testing.T) {
	_, source, _, dest, server := migrationStores(t)
	ctx := context.Background()
	stamp := formatMigrationTime(time.Now().UTC().Truncate(time.Second))
	execMigrationSQL(t, source, `INSERT INTO traffic_usage_ledger VALUES(?,?,?,?,?)`, server.ID, "epoch-live", "18446744073709551615", "host", "combined")
	execMigrationSQL(t, source, `INSERT INTO traffic_usage_tombstones VALUES(?,?,?,?,?,?)`, server.ID, "epoch-live", "host", "combined", "0", "9")
	execMigrationSQL(t, source, `INSERT INTO rollup_rebuild_queue VALUES(?,?,?)`, server.ID, stamp, 60)
	execMigrationSQL(t, source, `INSERT INTO traffic_allowances VALUES(?,?,?,?,?,?,?,?)`, server.ID, "host", "combined", "[]", "1000", 1, "UTC", "[]")
	execMigrationSQL(t, source, `INSERT INTO traffic_allowance_changes VALUES(?,?,?,?,?,?)`, server.ID, "host", "combined", stamp, "{}", "{}")
	execMigrationSQL(t, source, `INSERT INTO traffic_allowance_requests VALUES(?,?,?,?,?)`, server.ID, "request-1", "hash", "{}", stamp)
	artifact := replayArtifact(t, source, server.ID)
	if _, err := ImportMigration(ctx, dest, artifact, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	tx, err := dest.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, seq := range []uint64{5, ^uint64(0)} {
		applied, err := monitoring.TrafficUsageAlreadyAppliedTx(ctx, tx, server.ID, "epoch-live", seq, "host", "combined")
		if err != nil || !applied {
			t.Errorf("already charged seq=%d lost: %v %v", seq, applied, err)
		}
	}
	for _, table := range []string{"rollup_rebuild_queue", "traffic_allowances", "traffic_allowance_changes", "traffic_allowance_requests"} {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE server_id=?`, server.ID).Scan(&count); err != nil || count != 1 {
			t.Errorf("pending state %s count=%d err=%v", table, count, err)
		}
	}
}

func TestReplayMigrationRejectsSaturationCollisionAndMalformedAuthority(t *testing.T) {
	_, source, _, dest, server := migrationStores(t)
	ctx := context.Background()
	first := replayArtifact(t, source, server.ID)
	execMigrationSQL(t, source, `UPDATE collector_epoch_retirement_authority SET saturated=1 WHERE singleton=1`)
	if _, err := ImportMigration(ctx, dest, replayArtifact(t, source, server.ID), ImportOptions{}); err == nil {
		t.Fatal("source global saturation changed unrelated destination authority")
	}
	payload, err := unwrapMigrationPayload(first, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"table", "foreign-server", "missing", "frontier", "secret"} {
		t.Run(kind, func(t *testing.T) {
			data, _ := json.Marshal(payload)
			var changed MigrationExport
			if err := json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "table":
				changed.ReplayState[0].Table = "jobs"
			case "foreign-server":
				id, epoch, frontier, point := "other-0123456789", "epoch", "0", "0"
				changed.ReplayState[1].Rows = [][]*string{{&id, &epoch, &frontier, &point}}
			case "missing":
				changed.ReplayState = changed.ReplayState[1:]
			case "frontier":
				id, epoch, frontier, point := string(server.ID), "epoch", "9", "0"
				changed.ReplayState[1].Rows = [][]*string{{&id, &epoch, &frontier, &point}}
			case "secret":
				changed.BrowserAuthState = []byte(`{"credential":"forbidden"}`)
			}
			data, _ = json.Marshal(changed)
			artifact, err := wrapMigrationPayload(data, changed.Kind, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ImportMigration(ctx, dest, artifact, ImportOptions{}); err == nil {
				t.Fatal("malformed replay authority accepted")
			}
		})
	}
}

func TestReplayMigrationVersionCompatibilityEncryptionAndExplicitBaseline(t *testing.T) {
	_, source, _, dest, server := migrationStores(t)
	ctx := context.Background()
	first := replayArtifact(t, source, server.ID)
	if _, err := ImportMigration(ctx, dest, first, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	second := replayArtifact(t, source, server.ID)
	if _, err := ImportMigration(ctx, dest, second, ImportOptions{}); err == nil {
		t.Fatal("tail overwrote existing identity without explicit baseline")
	}
	if _, err := ImportMigration(ctx, dest, second, ImportOptions{PreviousArtifact: first}); err != nil {
		t.Fatal(err)
	}
	if result, err := ImportMigration(ctx, dest, first, ImportOptions{}); err != nil || !result.AlreadyApplied {
		t.Fatalf("old artifact retry changed final state: %+v %v", result, err)
	}
	if _, err := ExportMigration(ctx, source, ExportOptions{ServerID: server.ID, ReplaySafe: true, From: time.Now()}); err == nil {
		t.Fatal("range export claimed complete replay authority")
	}
	// The original v1 wire/encryption contract remains usable.
	legacy, err := ExportMigration(ctx, source, ExportOptions{ServerID: server.ID})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unwrapMigrationPayload(legacy, "")
	if err != nil || decoded.Format != MigrationFormat || decoded.SchemaVersion != MigrationSchemaVersion {
		t.Fatalf("legacy format changed: %+v %v", decoded, err)
	}
	_, _, _, fresh, _ := migrationStores(t)
	if _, err := ImportMigration(ctx, fresh, legacy, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	// Full-hub replay history is encrypted and excludes accounts/enrollment CA.
	execMigrationSQL(t, source, `INSERT INTO browser_auth_state VALUES(1,?)`, []byte(`{"credential":"excluded"}`))
	execMigrationSQL(t, source, `INSERT INTO fleet_identity_state VALUES(1,?)`, []byte(`{"private_key":"excluded"}`))
	password := "correct horse battery staple"
	encrypted, err := ExportMigration(ctx, source, ExportOptions{Kind: ExportFullHub, ReplaySafe: true, Passphrase: password})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = unwrapMigrationPayload(encrypted, password)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Format != MigrationReplayFormat || len(decoded.BrowserAuthState) > 0 || len(decoded.FleetIdentityState) > 0 {
		t.Fatal("v2 history included controller authority")
	}
	_, _, _, fullDest, _ := migrationStores(t)
	if _, err := ImportMigration(ctx, fullDest, encrypted, ImportOptions{Passphrase: password}); err != nil {
		t.Fatal(err)
	}
	var envelope migrationEnvelope
	if err := json.Unmarshal(encrypted, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Format = MigrationFormat
	tampered, _ := json.Marshal(envelope)
	if _, err := unwrapMigrationPayload(tampered, password); err == nil {
		t.Fatal("envelope format downgrade escaped authenticated binding")
	}
}

func TestReplayMigrationPreservesMaximumSequenceAcknowledgement(t *testing.T) {
	sourceStore, source, destStore, dest, server := migrationStores(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "max-epoch", Sequence: ^uint64(0), ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": .1}}
	gaps := []contracts.CoverageGap{{CollectorEpoch: sample.CollectorEpoch, FromSequence: 0, ToSequence: ^uint64(0), Reason: "transport-disconnect"}}
	if _, err := sourceStore.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, gaps); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportMigration(ctx, dest, replayArtifact(t, source, server.ID), ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, err := destStore.SequenceFrontierReady(ctx, server.ID, sample.CollectorEpoch, ^uint64(0))
	if err != nil || !ready {
		t.Fatalf("maximum sequence frontier lost: ready=%v err=%v", ready, err)
	}
	var count int
	if err := dest.QueryRow(`SELECT COUNT(*) FROM post_process_queue WHERE server_id=?`, server.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("maximum-sequence pending sample lost: count=%d err=%v", count, err)
	}
}

func TestReplayMigrationRejectsMalformedRegistryMetadataWithoutWrites(t *testing.T) {
	for _, kind := range []string{"duplicate", "text", "fractional", "null"} {
		t.Run(kind, func(t *testing.T) {
			_, source, _, dest, server := migrationStores(t)
			ctx := context.Background()
			artifact := replayArtifact(t, source, server.ID)
			corrupt := func(db *sql.DB) {
				execMigrationSQL(t, db, `DROP TABLE schema_meta`)
				execMigrationSQL(t, db, `CREATE TABLE schema_meta(version)`)
				switch kind {
				case "duplicate":
					execMigrationSQL(t, db, `INSERT INTO schema_meta VALUES(6),(6)`)
				case "text":
					execMigrationSQL(t, db, `INSERT INTO schema_meta VALUES('6')`)
				case "fractional":
					execMigrationSQL(t, db, `INSERT INTO schema_meta VALUES(6.5)`)
				case "null":
					execMigrationSQL(t, db, `INSERT INTO schema_meta VALUES(NULL)`)
				}
			}
			corrupt(source)
			if _, err := ExportMigration(ctx, source, ExportOptions{ServerID: server.ID, ReplaySafe: true}); err == nil {
				t.Fatal("invalid source metadata was coerced into supported schema")
			}
			corrupt(dest)
			if _, err := ImportMigration(ctx, dest, artifact, ImportOptions{}); err == nil {
				t.Fatal("invalid destination metadata was coerced into supported schema")
			}
			var servers, journal int
			if err := dest.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&servers); err != nil {
				t.Fatal(err)
			}
			if err := dest.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migration_imports'`).Scan(&journal); err != nil {
				t.Fatal(err)
			}
			if servers != 0 || journal != 0 {
				t.Fatalf("failed import changed state: servers=%d journal=%d", servers, journal)
			}
		})
	}
}

func TestReplayMigrationRetryStillValidatesDestinationSchema(t *testing.T) {
	for _, kind := range []string{"duplicate", "newer"} {
		t.Run(kind, func(t *testing.T) {
			_, source, _, dest, server := migrationStores(t)
			artifact := replayArtifact(t, source, server.ID)
			if _, err := ImportMigration(t.Context(), dest, artifact, ImportOptions{}); err != nil {
				t.Fatal(err)
			}
			if kind == "duplicate" {
				execMigrationSQL(t, dest, fmt.Sprintf(`INSERT INTO schema_meta VALUES(%d)`, monitoring.CurrentSchemaVersion))
			} else {
				execMigrationSQL(t, dest, fmt.Sprintf(`UPDATE schema_meta SET version=%d`, monitoring.CurrentSchemaVersion+1))
			}
			if _, err := ImportMigration(t.Context(), dest, artifact, ImportOptions{}); err == nil {
				t.Fatal("retry bypassed destination schema validation")
			}
		})
	}
}

func TestReplayMigrationTailCannotForgetCommittedAuthority(t *testing.T) {
	for _, kind := range []string{"raw", "charge-ledger", "charge-range"} {
		t.Run(kind, func(t *testing.T) {
			_, source, _, dest, server := migrationStores(t)
			table := "metric_sample_tombstones"
			switch kind {
			case "raw":
				execMigrationSQL(t, source, `INSERT INTO metric_sample_tombstones VALUES(?,?,?,?)`, server.ID, "epoch", "4", "9")
			case "charge-ledger":
				table = "traffic_usage_ledger"
				execMigrationSQL(t, source, `INSERT INTO traffic_usage_ledger VALUES(?,?,?,?,?)`, server.ID, "epoch", "5", "host", "combined")
			case "charge-range":
				table = "traffic_usage_tombstones"
				execMigrationSQL(t, source, `INSERT INTO traffic_usage_tombstones VALUES(?,?,?,?,?,?)`, server.ID, "epoch", "host", "combined", "4", "9")
			}
			first := replayArtifact(t, source, server.ID)
			if _, err := ImportMigration(context.Background(), dest, first, ImportOptions{}); err != nil {
				t.Fatal(err)
			}
			execMigrationSQL(t, source, `DELETE FROM `+table)
			before, _ := unwrapMigrationPayload(replayArtifact(t, dest, server.ID), "")
			if _, err := ImportMigration(context.Background(), dest, replayArtifact(t, source, server.ID), ImportOptions{PreviousArtifact: first}); err == nil {
				t.Fatal("tail erased committed replay/charge authority")
			}
			after, _ := unwrapMigrationPayload(replayArtifact(t, dest, server.ID), "")
			if !sameReplaySnapshot(before, after) {
				t.Fatal("rejected tail changed destination")
			}
		})
	}
}

func TestReplayMigrationAuthorityCompactionAndCoverage(t *testing.T) {
	for _, authority := range []string{"raw", "charged"} {
		for _, change := range []string{"split", "merge", "retire", "hole", "shrink", "wrong-scope", "wrong-epoch"} {
			if authority == "raw" && change == "wrong-scope" {
				continue
			}
			t.Run(authority+"/"+change, func(t *testing.T) {
				_, source, _, dest, server := migrationStores(t)
				table := "metric_sample_tombstones"
				if authority == "charged" {
					table = "traffic_usage_tombstones"
				}
				insert := func(from, to string) {
					if authority == "raw" {
						execMigrationSQL(t, source, `INSERT INTO metric_sample_tombstones VALUES(?,?,?,?)`, server.ID, "epoch", from, to)
					} else {
						execMigrationSQL(t, source, `INSERT INTO traffic_usage_tombstones VALUES(?,?,?,?,?,?)`, server.ID, "epoch", "host", "combined", from, to)
					}
				}
				if change == "merge" {
					insert("4", "6")
					insert("7", "9")
				} else {
					insert("4", "9")
				}
				// Inclusive upper endpoints must remain correct at the uint64 boundary.
				insert("18446744073709551614", "18446744073709551615")
				if authority == "charged" {
					execMigrationSQL(t, source, `INSERT INTO traffic_usage_ledger VALUES(?,?,?,?,?)`, server.ID, "epoch", "10", "host", "combined")
				}
				first := replayArtifact(t, source, server.ID)
				if _, err := ImportMigration(context.Background(), dest, first, ImportOptions{}); err != nil {
					t.Fatal(err)
				}
				before, _ := unwrapMigrationPayload(replayArtifact(t, dest, server.ID), "")
				execMigrationSQL(t, source, `DELETE FROM `+table)
				if authority == "charged" {
					execMigrationSQL(t, source, `DELETE FROM traffic_usage_ledger`)
				}
				switch change {
				case "split":
					insert("4", "6")
					insert("7", "10")
				case "merge":
					insert("3", "10")
				case "retire":
					execMigrationSQL(t, source, `INSERT INTO collector_epoch_retirements VALUES(?,?,?)`, server.ID, "epoch", formatMigrationTime(time.Now()))
				case "hole":
					insert("4", "6")
					insert("8", "10")
				case "shrink":
					insert("5", "10")
				case "wrong-scope":
					insert("4", "10")
					execMigrationSQL(t, source, `UPDATE traffic_usage_tombstones SET scope='interface:eth0'`)
				case "wrong-epoch":
					insert("4", "10")
					execMigrationSQL(t, source, `UPDATE `+table+` SET collector_epoch='other-epoch'`)
				}
				if change != "retire" {
					insert("18446744073709551614", "18446744073709551615")
				}
				_, err := ImportMigration(context.Background(), dest, replayArtifact(t, source, server.ID), ImportOptions{PreviousArtifact: first})
				valid := change == "split" || change == "merge" || change == "retire"
				if valid && err != nil {
					t.Fatalf("valid compaction rejected: %v", err)
				}
				if !valid && err == nil {
					t.Fatal("authority hole accepted")
				}
				if !valid {
					after, _ := unwrapMigrationPayload(replayArtifact(t, dest, server.ID), "")
					if !sameReplaySnapshot(before, after) {
						t.Fatal("rejected compaction changed destination")
					}
				}
				if valid && authority == "charged" {
					tx, err := dest.BeginTx(context.Background(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					for _, seq := range []uint64{4, 9, 10, ^uint64(0)} {
						applied, err := monitoring.TrafficUsageAlreadyAppliedTx(context.Background(), tx, server.ID, "epoch", seq, "host", "combined")
						if err != nil || !applied {
							t.Fatalf("charged authority lost for %d: %v", seq, err)
						}
					}
				}
			})
		}
	}
}

func TestReplayMigrationPreservesSparseMaximumPoint(t *testing.T) {
	sourceStore, source, destStore, dest, server := migrationStores(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "sparse-epoch", Sequence: ^uint64(0), ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": .1}}
	if _, err := sourceStore.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportMigration(ctx, dest, replayArtifact(t, source, server.ID), ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, err := destStore.SequenceFrontierReady(ctx, server.ID, sample.CollectorEpoch, ^uint64(0))
	if err != nil || ready {
		t.Fatalf("sparse maximum point falsely acknowledged: ready=%v err=%v", ready, err)
	}
	var frontier string
	var point int
	if err := dest.QueryRow(`SELECT frontier,max_point FROM sequence_frontiers WHERE server_id=?`, server.ID).Scan(&frontier, &point); err != nil {
		t.Fatal(err)
	}
	if frontier != "0" || point != 1 {
		t.Fatalf("sparse maximum state lost: %s %d", frontier, point)
	}
}

func TestReplayMigrationRejectsForgedFrontierClaims(t *testing.T) {
	for _, kind := range []string{"half-open-gap", "unsupported-max-point"} {
		t.Run(kind, func(t *testing.T) {
			sourceStore, source, _, dest, server := migrationStores(t)
			ctx := context.Background()
			if _, err := sourceStore.IngestSamples(ctx, server.ID, nil, []contracts.CoverageGap{{CollectorEpoch: "epoch", FromSequence: 0, ToSequence: 3, Reason: "transport-disconnect"}}); err != nil {
				t.Fatal(err)
			}
			artifact := replayArtifact(t, source, server.ID)
			payload, err := unwrapMigrationPayload(artifact, "")
			if err != nil {
				t.Fatal(err)
			}
			frontier := replayRows(payload, "sequence_frontiers")[0]
			column, value := 2, "4"
			if kind == "unsupported-max-point" {
				column, value = 3, "1"
			}
			frontier[column] = &value
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			forged, err := wrapMigrationPayload(data, payload.Kind, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ImportMigration(ctx, dest, forged, ImportOptions{}); err == nil {
				t.Fatal("forged frontier artifact imported")
			}
			if kind == "half-open-gap" {
				execMigrationSQL(t, source, `UPDATE sequence_frontiers SET frontier='4' WHERE server_id=?`, server.ID)
			} else {
				execMigrationSQL(t, source, `UPDATE sequence_frontiers SET max_point=1 WHERE server_id=?`, server.ID)
			}
			if _, err := ExportMigration(ctx, source, ExportOptions{ServerID: server.ID, ReplaySafe: true}); err == nil {
				t.Fatal("forged frontier was exported")
			}
			var count int
			if err := dest.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("destination changed: count=%d err=%v", count, err)
			}
		})
	}
}
