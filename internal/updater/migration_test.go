package updater

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	_ "modernc.org/sqlite"
)

func TestSingleServerMigrationFiltersSecretsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	sourcePath := t.TempDir() + "/source.db"
	source, err := monitoring.OpenStore(ctx, sourcePath, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	server := contracts.Server{ID: "server-0123456789", Name: "node-a", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, Version: "1.0.0", ConnectionState: "connected", FreshnessState: "fresh"}
	if err := source.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Truncate(time.Second)
	sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "epoch-0123456789", Sequence: 1, ObservedAt: observed, ReceivedAt: observed.Add(time.Second), Values: map[string]float64{"cpu.utilization": .5}, Counters: map[string]string{"net.eth0.rx_bytes": "42"}, Units: map[string]string{"cpu.utilization": "ratio"}, Validity: map[string]string{"cpu.utilization": "valid"}}
	if _, err := source.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, []contracts.CoverageGap{{CollectorEpoch: sample.CollectorEpoch, FromSequence: 2, ToSequence: 4, Reason: "transport-disconnect"}}); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveAuthState([]byte(`{"password":"do-not-export"}`)); err != nil {
		t.Fatal(err)
	}
	artifact, err := ExportMigration(ctx, openDB(t, sourcePath), ExportOptions{Kind: ExportSingleServer, ServerID: server.ID})
	if err != nil {
		t.Fatal(err)
	}
	var envelope migrationEnvelope
	if err := json.Unmarshal(artifact, &envelope); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawStdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(decoded), "do-not-export") || strings.Contains(string(decoded), "browser_auth_state") {
		t.Fatal("single-server export leaked auth state")
	}

	destinationPath := t.TempDir() + "/destination.db"
	destination, err := monitoring.OpenStore(ctx, destinationPath, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	result, err := ImportMigration(ctx, openDB(t, destinationPath), artifact, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Samples != 1 || result.AlreadyApplied {
		t.Fatalf("unexpected first import: %+v", result)
	}
	page, err := destination.QueryMetrics(ctx, server.ID, observed.Add(-time.Minute), observed.Add(time.Minute), 20, "")
	if err != nil || len(page.Samples) != 1 {
		t.Fatalf("imported samples=%d err=%v", len(page.Samples), err)
	}
	retry, err := ImportMigration(ctx, openDB(t, destinationPath), artifact, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !retry.AlreadyApplied {
		t.Fatalf("expected idempotent import, got %+v", retry)
	}
}

func TestFullHubMigrationRequiresEncryptionAndWrongPassphraseFails(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/hub.db"
	store, err := monitoring.OpenStore(ctx, path, monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := contracts.Server{ID: "server-0123456789", Name: "hub-node", Role: "node", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, ConnectionState: "never-connected", FreshnessState: "unknown"}
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuthState([]byte(`{"credential":"must-be-encrypted"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportMigration(ctx, openDB(t, path), ExportOptions{Kind: ExportFullHub}); err == nil {
		t.Fatal("unencrypted full-hub export accepted")
	}
	artifact, err := ExportMigration(ctx, openDB(t, path), ExportOptions{Kind: ExportFullHub, Passphrase: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(artifact), server.Name) {
		t.Fatal("full-hub plaintext leaked through envelope")
	}
	var envelope migrationEnvelope
	if err := json.Unmarshal(artifact, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Iterations = MigrationMaxKDFIterations + 1
	malformed, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwrapMigrationPayload(malformed, "correct horse battery staple"); err == nil {
		t.Fatal("excessive KDF iterations accepted")
	}
	envelope.Iterations = 1
	malformed, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwrapMigrationPayload(malformed, "correct horse battery staple"); err == nil {
		t.Fatal("insufficient KDF iterations accepted")
	}
	envelope.Iterations = MigrationKDFIterations
	envelope.KDF = "unknown-kdf"
	malformed, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwrapMigrationPayload(malformed, "correct horse battery staple"); err == nil {
		t.Fatal("unknown KDF accepted")
	}
	if _, err := ImportMigration(ctx, openDB(t, path), artifact, ImportOptions{Passphrase: "wrong passphrase"}); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
}

type failingRandomReader struct{}

func (failingRandomReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRandomExportIDFailsClosedOnEntropyFailure(t *testing.T) {
	original := cryptorand.Reader
	cryptorand.Reader = failingRandomReader{}
	t.Cleanup(func() { cryptorand.Reader = original })
	if id, err := randomExportID(); err == nil || id != "" {
		t.Fatalf("expected entropy failure, id=%q err=%v", id, err)
	}
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
