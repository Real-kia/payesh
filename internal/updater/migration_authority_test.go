package updater

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func authoritySamples(server contracts.ServerID, from, to uint64) []contracts.MetricSample {
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	out := make([]contracts.MetricSample, 0, to-from)
	for sequence := from; sequence < to; sequence++ {
		at := base.Add(time.Duration(sequence) * time.Second)
		out = append(out, contracts.MetricSample{ServerID: server, CollectorEpoch: "handoff-epoch-0001", Sequence: sequence, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": float64(sequence) + 0.5}})
	}
	return out
}

func authorityRequestDigest() string { return strings.Repeat("c", 64) }

// The destination only takes authority when the state it imported hashes to the
// exact frontier the source froze, using the real migration-v2 transfer path.
func TestAuthorityHandoffOverMigrationArtifact(t *testing.T) {
	ctx := context.Background()
	sourceStore, sourceDB, destStore, destDB, server := migrationStores(t)
	if _, err := sourceStore.IngestSamples(ctx, server.ID, authoritySamples(server.ID, 0, 3), nil); err != nil {
		t.Fatal(err)
	}
	initial := replayArtifact(t, sourceDB, server.ID)
	if _, err := ImportMigration(ctx, destDB, initial, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	// The source keeps collecting after the first transfer, then is frozen.
	if _, err := sourceStore.IngestSamples(ctx, server.ID, authoritySamples(server.ID, 3, 6), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStore.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	transition := monitoring.AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityRequestDigest()}
	if _, err := sourceStore.FreezeServerAuthority(ctx, transition); err != nil {
		t.Fatal(err)
	}
	relinquished, err := sourceStore.RelinquishServerAuthority(ctx, transition)
	if err != nil {
		t.Fatal(err)
	}
	handoff := monitoring.AuthorityHandoff{ServerID: server.ID, CutoverID: relinquished.CutoverID, SourceGeneration: relinquished.Generation, FrontierDigest: relinquished.FrontierDigest, SourceRequestDigest: relinquished.TransitionDigest, NewOwner: "owner-hub-bbbb"}
	// Only the first transfer arrived: the destination is missing the final tail.
	if _, err := destStore.ActivateServerAuthority(ctx, handoff); !errors.Is(err, monitoring.ErrAuthorityConflict) {
		t.Fatalf("destination activated without the final tail: %v", err)
	}
	// Transfer the frozen tail through the real incremental path, then activate.
	tail := replayArtifact(t, sourceDB, server.ID)
	if _, err := ImportMigration(ctx, destDB, tail, ImportOptions{PreviousArtifact: initial}); err != nil {
		t.Fatal(err)
	}
	active, err := destStore.ActivateServerAuthority(ctx, handoff)
	if err != nil {
		t.Fatalf("destination refused an exact transfer: %v", err)
	}
	if active.State != monitoring.AuthorityActive || active.Generation != relinquished.Generation+1 || active.FrontierDigest != relinquished.FrontierDigest {
		t.Fatalf("activated authority=%+v source=%+v", active, relinquished)
	}
	if _, err := destStore.IngestSamples(ctx, server.ID, authoritySamples(server.ID, 6, 7), nil); err != nil {
		t.Fatalf("the new owner cannot ingest: %v", err)
	}
	if _, err := sourceStore.IngestSamples(ctx, server.ID, authoritySamples(server.ID, 6, 7), nil); !errors.Is(err, monitoring.ErrServerNotAuthoritative) {
		t.Fatalf("the old owner still accepts ingest: %v", err)
	}
}
