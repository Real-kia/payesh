package updater

import (
	"context"
	"testing"
)

func TestInspectMigrationReportsServerScopeWithoutImporting(t *testing.T) {
	ctx := context.Background()
	sourceStore, sourceDB, _, destDB, server := migrationStores(t)
	if _, err := sourceStore.IngestSamples(ctx, server.ID, authoritySamples(server.ID, 0, 2), nil); err != nil {
		t.Fatal(err)
	}
	artifact := replayArtifact(t, sourceDB, server.ID)
	info, err := InspectMigration(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if info.SourceServerID != server.ID || len(info.ServerIDs) != 1 || info.ServerIDs[0] != server.ID || info.ExportID == "" || info.Format != MigrationReplayFormat {
		t.Fatalf("inspection=%+v", info)
	}
	var servers int
	if err := destDB.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&servers); err != nil || servers != 0 {
		t.Fatalf("inspection changed the destination: servers=%d err=%v", servers, err)
	}
}

func TestInspectMigrationRefusesGarbageAndOversizedArtifacts(t *testing.T) {
	if _, err := InspectMigration(nil); err == nil {
		t.Fatal("empty artifact accepted")
	}
	if _, err := InspectMigration([]byte("not a migration artifact")); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := InspectMigration(make([]byte, maxMigrationBytes+1)); err == nil {
		t.Fatal("oversized artifact accepted")
	}
}
