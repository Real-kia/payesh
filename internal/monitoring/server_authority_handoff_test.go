package monitoring

import (
	"context"
	"errors"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func handoffStore(t *testing.T, samples uint64) (*Store, contracts.Server) {
	t.Helper()
	store, server := openAuthorityStore(t)
	batch := make([]contracts.MetricSample, 0, samples)
	for sequence := uint64(0); sequence < samples; sequence++ {
		batch = append(batch, authoritySample(server.ID, sequence))
	}
	if len(batch) > 0 {
		if _, err := store.IngestSamples(context.Background(), server.ID, batch, nil); err != nil {
			t.Fatal(err)
		}
	}
	return store, server
}

func TestIngestFrontierDigestIsDeterministicAndContentSensitive(t *testing.T) {
	ctx := context.Background()
	a, serverA := handoffStore(t, 5)
	b, serverB := handoffStore(t, 5)
	digestA, err := a.IngestFrontierDigest(ctx, serverA.ID)
	if err != nil || !validAuthorityDigest(digestA) {
		t.Fatalf("digest=%q err=%v", digestA, err)
	}
	digestB, err := b.IngestFrontierDigest(ctx, serverB.ID)
	if err != nil || digestA != digestB {
		t.Fatalf("identical content must give identical digests: %q vs %q err=%v", digestA, digestB, err)
	}
	if again, _ := a.IngestFrontierDigest(ctx, serverA.ID); again != digestA {
		t.Fatal("digest is not stable across calls")
	}
	if _, err := b.IngestSamples(ctx, serverB.ID, []contracts.MetricSample{authoritySample(serverB.ID, 5)}, nil); err != nil {
		t.Fatal(err)
	}
	if changed, _ := b.IngestFrontierDigest(ctx, serverB.ID); changed == digestB {
		t.Fatal("an extra sample did not change the digest")
	}
	altered := authoritySample(serverA.ID, 2)
	altered.Values = map[string]float64{"cpu.utilization": 99}
	if _, err := a.db.ExecContext(ctx, `UPDATE metric_samples SET values_json=? WHERE server_id=? AND sequence='2'`, `{"cpu.utilization":99}`, string(serverA.ID)); err != nil {
		t.Fatal(err)
	}
	if changed, _ := a.IngestFrontierDigest(ctx, serverA.ID); changed == digestA {
		t.Fatal("changed sample content did not change the digest")
	}
}

func TestFreezeComputesTheFrontierDigestInsideItsTransaction(t *testing.T) {
	ctx := context.Background()
	store, server := handoffStore(t, 4)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	want, err := store.IngestFrontierDigest(ctx, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := store.FreezeServerAuthority(ctx, AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a")})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.FrontierDigest != want {
		t.Fatalf("freeze recorded %q, store frontier was %q", frozen.FrontierDigest, want)
	}
	relinquished, err := store.RelinquishServerAuthority(ctx, AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a")})
	if err != nil || relinquished.FrontierDigest != want || relinquished.State != AuthorityRelinquished {
		t.Fatalf("relinquish=%+v err=%v", relinquished, err)
	}
}

func relinquishedSource(t *testing.T, samples uint64) (*Store, contracts.Server, ServerAuthority) {
	t.Helper()
	ctx := context.Background()
	store, server := handoffStore(t, samples)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	transition := AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a")}
	if _, err := store.FreezeServerAuthority(ctx, transition); err != nil {
		t.Fatal(err)
	}
	done, err := store.RelinquishServerAuthority(ctx, transition)
	if err != nil {
		t.Fatal(err)
	}
	return store, server, done
}

func handoffFor(source ServerAuthority) AuthorityHandoff {
	return AuthorityHandoff{ServerID: source.ServerID, CutoverID: source.CutoverID, SourceGeneration: source.Generation, FrontierDigest: source.FrontierDigest, SourceRequestDigest: source.TransitionDigest, NewOwner: "owner-hub-bbbb"}
}

func TestDestinationActivatesOnlyWithTheExactTransferredFrontier(t *testing.T) {
	ctx := context.Background()
	_, _, source := relinquishedSource(t, 6)
	dest, destServer := handoffStore(t, 6)
	handoff := handoffFor(source)
	handoff.ServerID = destServer.ID
	active, err := dest.ActivateServerAuthority(ctx, handoff)
	if err != nil {
		t.Fatal(err)
	}
	if active.State != AuthorityActive || active.Owner != "owner-hub-bbbb" || active.Generation != source.Generation+1 || active.CutoverID != "cutover-0001" {
		t.Fatalf("activated authority=%+v (source generation %d)", active, source.Generation)
	}
	if _, err := dest.IngestSamples(ctx, destServer.ID, []contracts.MetricSample{authoritySample(destServer.ID, 6)}, nil); err != nil {
		t.Fatalf("new owner could not ingest after activation: %v", err)
	}
	again, err := dest.ActivateServerAuthority(ctx, handoff)
	if err != nil || again != active {
		t.Fatalf("exact replay=%+v err=%v want %+v", again, err, active)
	}
}

func TestDestinationRefusesAMissingOrExtraTail(t *testing.T) {
	ctx := context.Background()
	_, _, source := relinquishedSource(t, 6)
	for name, samples := range map[string]uint64{"missing-tail": 5, "extra-tail": 7} {
		t.Run(name, func(t *testing.T) {
			dest, destServer := handoffStore(t, samples)
			handoff := handoffFor(source)
			handoff.ServerID = destServer.ID
			if _, err := dest.ActivateServerAuthority(ctx, handoff); !errors.Is(err, ErrAuthorityConflict) {
				t.Fatalf("a destination with the wrong tail was activated: %v", err)
			}
			if _, found, err := dest.GetServerAuthority(ctx, destServer.ID); err != nil || found {
				t.Fatalf("refused activation left authority behind: found=%v err=%v", found, err)
			}
		})
	}
}

func TestActivationRefusesStaleConflictingAndMalformedHandoffs(t *testing.T) {
	ctx := context.Background()
	_, _, source := relinquishedSource(t, 3)
	dest, destServer := handoffStore(t, 3)
	handoff := handoffFor(source)
	handoff.ServerID = destServer.ID
	if _, err := dest.ActivateServerAuthority(ctx, handoff); err != nil {
		t.Fatal(err)
	}
	otherOwner := handoff
	otherOwner.NewOwner = "owner-hub-cccc"
	if _, err := dest.ActivateServerAuthority(ctx, otherOwner); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("a second owner for the same cutover must conflict: %v", err)
	}
	laterCutover := handoff
	laterCutover.CutoverID = "cutover-0002"
	if _, err := dest.ActivateServerAuthority(ctx, laterCutover); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("an active destination must not accept another cutover: %v", err)
	}
	for name, mutate := range map[string]func(*AuthorityHandoff){
		"bad-digest":   func(h *AuthorityHandoff) { h.FrontierDigest = "xyz" },
		"zero-gen":     func(h *AuthorityHandoff) { h.SourceGeneration = 0 },
		"bad-owner":    func(h *AuthorityHandoff) { h.NewOwner = "bad owner!" },
		"bad-cutover":  func(h *AuthorityHandoff) { h.CutoverID = "" },
		"bad-source":   func(h *AuthorityHandoff) { h.SourceRequestDigest = "nope" },
		"unregistered": func(h *AuthorityHandoff) { h.ServerID = "server-missing-0001" },
	} {
		t.Run(name, func(t *testing.T) {
			fresh, freshServer := handoffStore(t, 3)
			bad := handoffFor(source)
			bad.ServerID = freshServer.ID
			mutate(&bad)
			if _, err := fresh.ActivateServerAuthority(ctx, bad); err == nil {
				t.Fatal("malformed handoff accepted")
			}
		})
	}
}

func TestHandoffMovesWritesFromSourceToDestination(t *testing.T) {
	ctx := context.Background()
	sourceStore, sourceServer, source := relinquishedSource(t, 4)
	dest, destServer := handoffStore(t, 4)
	handoff := handoffFor(source)
	handoff.ServerID = destServer.ID
	if _, err := dest.ActivateServerAuthority(ctx, handoff); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStore.IngestSamples(ctx, sourceServer.ID, []contracts.MetricSample{authoritySample(sourceServer.ID, 4)}, nil); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("the source still accepts ingest after handoff: %v", err)
	}
	if _, err := dest.IngestSamples(ctx, destServer.ID, []contracts.MetricSample{authoritySample(destServer.ID, 4)}, nil); err != nil {
		t.Fatalf("the destination does not accept ingest after handoff: %v", err)
	}
}

func TestAbortedFreezeResumesTheSourceAndBurnsTheCutoverID(t *testing.T) {
	ctx := context.Background()
	store, server := handoffStore(t, 3)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	transition := AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a")}
	frozen, err := store.FreezeServerAuthority(ctx, transition)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 3)}, nil); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("frozen source accepted ingest: %v", err)
	}
	wrong := transition
	wrong.RequestDigest = authorityDigest("c")
	if _, err := store.AbortServerFreeze(ctx, wrong); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("abort with another request must conflict: %v", err)
	}
	aborted, err := store.AbortServerFreeze(ctx, transition)
	if err != nil {
		t.Fatal(err)
	}
	if aborted.State != AuthorityActive || aborted.Generation != frozen.Generation+1 || aborted.CutoverID != "" {
		t.Fatalf("aborted authority=%+v frozen=%+v", aborted, frozen)
	}
	again, err := store.AbortServerFreeze(ctx, transition)
	if err != nil || again != aborted {
		t.Fatalf("abort replay=%+v err=%v want %+v", again, err, aborted)
	}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 3)}, nil); err != nil {
		t.Fatalf("resumed source cannot ingest: %v", err)
	}
	if _, err := store.FreezeServerAuthority(ctx, transition); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("an aborted cutover id was reused: %v", err)
	}
	next := transition
	next.CutoverID = "cutover-0002"
	refrozen, err := store.FreezeServerAuthority(ctx, next)
	if err != nil || refrozen.Generation != aborted.Generation+1 {
		t.Fatalf("a new cutover could not freeze: %+v err=%v", refrozen, err)
	}
}

func TestAbortRefusedAfterRelinquish(t *testing.T) {
	ctx := context.Background()
	store, server, relinquished := relinquishedSource(t, 2)
	if _, err := store.AbortServerFreeze(ctx, AuthorityTransition{ServerID: server.ID, CutoverID: relinquished.CutoverID, RequestDigest: relinquished.TransitionDigest}); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("a relinquished server was un-frozen: %v", err)
	}
}
