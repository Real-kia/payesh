package updater

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

type cutoverFixture struct {
	source    *monitoring.Store
	sourceDB  *sql.DB
	dest      *monitoring.Store
	destDB    *sql.DB
	server    contracts.Server
	journal   *sql.DB
	stateDir  string
	cutover   *StoreCutover
	clockBase time.Time
}

func newCutoverFixture(t *testing.T) *cutoverFixture {
	t.Helper()
	ctx := context.Background()
	source, sourceDB, dest, destDB, server := migrationStores(t)
	if _, err := source.IngestSamples(ctx, server.ID, authoritySamples(server.ID, 0, 5), nil); err != nil {
		t.Fatal(err)
	}
	f := &cutoverFixture{source: source, sourceDB: sourceDB, dest: dest, destDB: destDB, server: server,
		journal: openDB(t, filepath.Join(t.TempDir(), "journal.db")), stateDir: t.TempDir(), clockBase: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	f.cutover = &StoreCutover{Source: source, SourceDB: sourceDB, Peer: &LocalStorePeer{DB: destDB, Store: dest}, ServerID: server.ID, StateDir: f.stateDir}
	return f
}

func cutoverRequest(id string) RoleCutoverRequest {
	return RoleCutoverRequest{ID: id, SourceRole: "standalone", DestinationRole: "node", SourceID: "owner-hub-aaaa", DestinationID: "owner-hub-bbbb"}
}

func (f *cutoverFixture) assertCompleted(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	src, found, err := f.source.GetServerAuthority(ctx, f.server.ID)
	if err != nil || !found || src.State != monitoring.AuthorityRelinquished || src.CutoverID != id {
		t.Fatalf("source authority=%+v found=%v err=%v", src, found, err)
	}
	dst, found, err := f.dest.GetServerAuthority(ctx, f.server.ID)
	if err != nil || !found || dst.State != monitoring.AuthorityActive || dst.Owner != "owner-hub-bbbb" || dst.CutoverID != id || dst.Generation != src.Generation+1 || dst.FrontierDigest != src.FrontierDigest {
		t.Fatalf("destination authority=%+v source=%+v found=%v err=%v", dst, src, found, err)
	}
	digest, err := f.dest.IngestFrontierDigest(ctx, f.server.ID)
	if err != nil || digest != src.FrontierDigest {
		t.Fatalf("destination state digest %q != frozen source digest %q (err=%v)", digest, src.FrontierDigest, err)
	}
	if _, err := f.source.IngestSamples(ctx, f.server.ID, authoritySamples(f.server.ID, 900, 901), nil); !errors.Is(err, monitoring.ErrServerNotAuthoritative) {
		t.Fatalf("the old owner still ingests: %v", err)
	}
	if _, err := f.dest.IngestSamples(ctx, f.server.ID, authoritySamples(f.server.ID, 900, 901), nil); err != nil {
		t.Fatalf("the new owner cannot ingest: %v", err)
	}
}

func TestStoreCutoverMovesAuthorityThroughEveryPhase(t *testing.T) {
	f := newCutoverFixture(t)
	req := cutoverRequest("cutover-0001")
	record, err := RunRoleCutover(context.Background(), f.journal, req, f.cutover.Hooks(), func() time.Time { return f.clockBase })
	if err != nil || record.Phase != CutoverComplete {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	f.assertCompleted(t, req.ID)
}

// A crash after any phase's side effects (a panic that leaves the lease held)
// must resume from that same phase once the lease expires, with idempotent
// hooks reaching the same final state.
func TestStoreCutoverResumesAfterACrashInEveryPhase(t *testing.T) {
	phases := []string{"preflight", "backup", "transfer", "verify", "freeze", "tail", "switch", "revoke", "confirm"}
	for _, crashAt := range phases {
		t.Run(crashAt, func(t *testing.T) {
			f := newCutoverFixture(t)
			req := cutoverRequest("cutover-0002")
			hooks := f.cutover.Hooks()
			crash := func(real func(context.Context, RoleCutoverRequest) error) func(context.Context, RoleCutoverRequest) error {
				return func(ctx context.Context, r RoleCutoverRequest) error {
					if err := real(ctx, r); err != nil {
						return err
					}
					panic("simulated process crash after side effects")
				}
			}
			crashing := hooks
			switch crashAt {
			case "preflight":
				crashing.Preflight = crash(hooks.Preflight)
			case "backup":
				crashing.Backup = crash(hooks.Backup)
			case "transfer":
				crashing.Transfer = crash(hooks.Transfer)
			case "verify":
				crashing.Verify = crash(hooks.Verify)
			case "freeze":
				crashing.Freeze = crash(hooks.Freeze)
			case "tail":
				crashing.TransferTail = crash(hooks.TransferTail)
			case "switch":
				crashing.SwitchAuthority = crash(hooks.SwitchAuthority)
			case "revoke":
				crashing.RevokeOldAuthority = crash(hooks.RevokeOldAuthority)
			case "confirm":
				crashing.Confirm = crash(hooks.Confirm)
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the injected crash did not happen")
					}
				}()
				_, _ = RunRoleCutover(context.Background(), f.journal, req, crashing, func() time.Time { return f.clockBase })
			}()
			resumedAt := f.clockBase.Add(cutoverLeaseDuration + time.Minute)
			record, err := RunRoleCutover(context.Background(), f.journal, req, f.cutover.Hooks(), func() time.Time { return resumedAt })
			if err != nil || record.Phase != CutoverComplete {
				t.Fatalf("resume after crash in %s: record=%+v err=%v", crashAt, record, err)
			}
			f.assertCompleted(t, req.ID)
		})
	}
}

// A failure before authority switches must leave the source writing again, give
// the destination no authority, and burn the failed cutover identifier.
func TestStoreCutoverFailureBeforeSwitchResumesTheSource(t *testing.T) {
	ctx := context.Background()
	f := newCutoverFixture(t)
	req := cutoverRequest("cutover-0003")
	hooks := f.cutover.Hooks()
	hooks.TransferTail = func(context.Context, RoleCutoverRequest) error { return errors.New("peer unreachable") }
	record, err := RunRoleCutover(ctx, f.journal, req, hooks, func() time.Time { return f.clockBase })
	if err == nil || record.Phase != CutoverPreflight {
		t.Fatalf("failed cutover record=%+v err=%v", record, err)
	}
	src, found, err := f.source.GetServerAuthority(ctx, f.server.ID)
	if err != nil || !found || src.State != monitoring.AuthorityActive {
		t.Fatalf("source was not resumed: %+v found=%v err=%v", src, found, err)
	}
	if _, found, err := f.dest.GetServerAuthority(ctx, f.server.ID); err != nil || found {
		t.Fatalf("the destination gained authority from a failed cutover: found=%v err=%v", found, err)
	}
	if _, err := f.source.IngestSamples(ctx, f.server.ID, authoritySamples(f.server.ID, 5, 7), nil); err != nil {
		t.Fatalf("the resumed source cannot ingest: %v", err)
	}
	if _, err := RunRoleCutover(ctx, f.journal, req, f.cutover.Hooks(), func() time.Time { return f.clockBase.Add(time.Hour) }); err == nil {
		t.Fatal("a burned cutover identifier was allowed to run again")
	}
	// A new cutover identifier succeeds, importing over the baseline that the
	// failed attempt left on the destination.
	retry := cutoverRequest("cutover-0004")
	record, err = RunRoleCutover(ctx, f.journal, retry, f.cutover.Hooks(), func() time.Time { return f.clockBase.Add(2 * time.Hour) })
	if err != nil || record.Phase != CutoverComplete {
		t.Fatalf("retry with a new identifier: record=%+v err=%v", record, err)
	}
	f.assertCompleted(t, retry.ID)
}

func TestStoreCutoverRefusesAnUnsafeConfiguration(t *testing.T) {
	f := newCutoverFixture(t)
	for name, mutate := range map[string]func(*StoreCutover){
		"no-source":  func(c *StoreCutover) { c.Source = nil },
		"no-peer":    func(c *StoreCutover) { c.Peer = nil },
		"bad-server": func(c *StoreCutover) { c.ServerID = "x" },
		"no-state":   func(c *StoreCutover) { c.StateDir = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *f.cutover
			mutate(&bad)
			if _, err := RunRoleCutover(context.Background(), f.journal, cutoverRequest("cutover-0005"), bad.Hooks(), func() time.Time { return f.clockBase }); err == nil {
				t.Fatal("unsafe cutover configuration ran")
			}
		})
	}
}

func TestStoreCutoverRevokesTheOldHubIdentityAfterRelinquishing(t *testing.T) {
	ctx := context.Background()
	f := newCutoverFixture(t)
	var revoked []contracts.ServerID
	var stateAtRevoke monitoring.AuthorityState
	f.cutover.RevokeSourceIdentity = func(ctx context.Context, id contracts.ServerID) error {
		revoked = append(revoked, id)
		current, _, err := f.source.GetServerAuthority(ctx, id)
		stateAtRevoke = current.State
		return err
	}
	record, err := RunRoleCutover(ctx, f.journal, cutoverRequest("cutover-0006"), f.cutover.Hooks(), func() time.Time { return f.clockBase })
	if err != nil || record.Phase != CutoverComplete {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	if len(revoked) != 1 || revoked[0] != f.server.ID {
		t.Fatalf("identity revocations=%v", revoked)
	}
	if stateAtRevoke != monitoring.AuthorityRelinquished {
		t.Fatalf("the identity was revoked while the source was %q, not relinquished", stateAtRevoke)
	}
}

// A revocation failure after the switch cannot be rolled back, so the journal
// must stop in operator recovery with the reason recorded.
func TestStoreCutoverRevocationFailureRequiresOperatorRecovery(t *testing.T) {
	ctx := context.Background()
	f := newCutoverFixture(t)
	f.cutover.RevokeSourceIdentity = func(context.Context, contracts.ServerID) error {
		return errors.New("certificate authority unavailable")
	}
	record, err := RunRoleCutover(ctx, f.journal, cutoverRequest("cutover-0007"), f.cutover.Hooks(), func() time.Time { return f.clockBase })
	if err == nil || record.Phase != CutoverRecovery || record.LastError == "" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	dst, found, err := f.dest.GetServerAuthority(ctx, f.server.ID)
	if err != nil || !found || dst.State != monitoring.AuthorityActive {
		t.Fatalf("the destination lost authority after a revocation failure: %+v found=%v err=%v", dst, found, err)
	}
}

func TestLoadRoleCutoverReportsTheJournalRecord(t *testing.T) {
	ctx := context.Background()
	f := newCutoverFixture(t)
	if _, found, err := LoadRoleCutover(ctx, f.journal, "cutover-0099"); err != nil || found {
		t.Fatalf("a journal with no table must report not found: found=%v err=%v", found, err)
	}
	req := cutoverRequest("cutover-0008")
	if _, err := RunRoleCutover(ctx, f.journal, req, f.cutover.Hooks(), func() time.Time { return f.clockBase }); err != nil {
		t.Fatal(err)
	}
	record, found, err := LoadRoleCutover(ctx, f.journal, req.ID)
	if err != nil || !found || record.Phase != CutoverComplete || record.Request != req {
		t.Fatalf("record=%+v found=%v err=%v", record, found, err)
	}
	if _, found, err := LoadRoleCutover(ctx, f.journal, "cutover-0099"); err != nil || found {
		t.Fatalf("an unknown cutover must report not found: found=%v err=%v", found, err)
	}
}
