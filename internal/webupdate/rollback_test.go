package webupdate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rollbackFixture(t *testing.T) (InstallationTransaction, map[string]string, *[]string) {
	t.Helper()
	root := t.TempDir()
	calls := []string{}
	contents := map[string]string{"/usr/bin/payesh": "old-version", "/usr/bin/payesh-agent": "old-agent", "/etc/payesh/payesh.env": "original-config", "/etc/systemd/system/payesh-agent.service": "old-unit", "/var/lib/payesh/install-state.json": `{"role":"node","init":"systemd"}`, "/usr/share/payesh/web-assets/index.html": "old-dashboard"}
	for path, content := range contents {
		p := filepath.Join(root, strings.TrimPrefix(path, "/"))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteInstallationScope(root, InstallationScope{Role: "node", Init: "systemd"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "var/lib/payesh/payesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE snapshot(value TEXT); INSERT INTO snapshot VALUES('old-data')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	txn := InstallationTransaction{Root: root, JobID: "rollback-installed-test01", Services: func(_ context.Context, action, role, init string) error {
		if role != "node" || init != "systemd" {
			t.Fatalf("untrusted service scope %s/%s", role, init)
		}
		calls = append(calls, action)
		return nil
	}}
	return txn, contents, &calls
}

func TestInstalledLayoutRollbackRestoresVersionConfigStateServicesAndSQLite(t *testing.T) {
	txn, contents, calls := rollbackFixture(t)
	ctx := context.Background()
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	for path := range contents {
		if err := os.WriteFile(txn.path(path), []byte("new-installation"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(txn.path("/usr/bin/payesh-server"), []byte("newly-added"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txn.path("/var/lib/payesh/payesh.db"), []byte("damaged-candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txn.path("/var/lib/payesh/payesh.db-wal"), []byte("stale-wal"), 0600); err != nil {
		t.Fatal(err)
	}
	// Service-writable install state cannot redirect restore service selection.
	if err := os.WriteFile(txn.path("/var/lib/payesh/install-state.json"), []byte(`{"role":"hub","init":"openrc"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := txn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for path, want := range contents {
		p := txn.path(path)
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Fatalf("path=%s got=%q err=%v", path, got, err)
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0640 {
			t.Fatalf("lost mode at %s", path)
		}
	}
	if _, err := os.Stat(txn.path("/usr/bin/payesh-server")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("newly added candidate artifact was not removed")
	}
	db, err := sql.Open("sqlite", "file:"+txn.path("/var/lib/payesh/payesh.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow("SELECT value FROM snapshot").Scan(&value); err != nil || value != "old-data" {
		t.Fatalf("restored data=%q err=%v", value, err)
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "stop", "reload", "start"}) {
		t.Fatalf("service sequence=%v", *calls)
	}
	phase, err := txn.Phase()
	if err != nil || phase != "rolled-back" {
		t.Fatalf("phase=%q err=%v", phase, err)
	}
}

func TestRollbackRetainsFailedGenerationBeforeRestoring(t *testing.T) {
	txn, _, _ := rollbackFixture(t)
	ctx := context.Background()
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"payesh.db": "candidate-data", "payesh.db-wal": "candidate-wal", "payesh.db-shm": "candidate-shm", "agent.spool": "candidate-spool", "node-identity.json": "candidate-identity"} {
		if err := os.WriteFile(txn.path("/var/lib/payesh/"+name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := txn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"payesh.db": "candidate-data", "payesh.db-wal": "candidate-wal", "payesh.db-shm": "candidate-shm", "agent.spool": "candidate-spool", "node-identity.json": "candidate-identity"} {
		got, err := os.ReadFile(filepath.Join(txn.dir(), "failed-generation", name))
		if err != nil || string(got) != want {
			t.Fatalf("lost failed-generation %s: got=%q err=%v", name, got, err)
		}
	}
	if err := txn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(txn.dir(), "failed-generation", "payesh.db"))
	if err != nil || string(got) != "candidate-data" {
		t.Fatalf("retry overwrote retained candidate: %q %v", got, err)
	}
}

func TestRollbackRefusesRestoreWhenFailedGenerationCannotBeCaptured(t *testing.T) {
	txn, _, calls := rollbackFixture(t)
	ctx := context.Background()
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txn.path("/var/lib/payesh/payesh.db"), []byte("candidate-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(txn.path("/var/lib/payesh/payesh.db"), txn.path("/var/lib/payesh/agent.spool")); err != nil {
		t.Fatal(err)
	}
	if err := txn.Rollback(ctx); err == nil {
		t.Fatal("restore proceeded without preserving failed generation")
	}
	got, err := os.ReadFile(txn.path("/var/lib/payesh/payesh.db"))
	if err != nil || string(got) != "candidate-data" {
		t.Fatalf("candidate overwritten on capture failure: %q %v", got, err)
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "stop"}) {
		t.Fatalf("services restarted after unsafe capture: %v", *calls)
	}
	if err := os.Remove(txn.path("/var/lib/payesh/agent.spool")); err != nil {
		t.Fatal(err)
	}
	if recovered, err := txn.Recover(ctx); err != nil || !recovered {
		t.Fatalf("capture failure could not resume: %v %v", recovered, err)
	}
	got, err = os.ReadFile(filepath.Join(txn.dir(), "failed-generation", "payesh.db"))
	if err != nil || string(got) != "candidate-data" {
		t.Fatalf("retry lost original failed data: %q %v", got, err)
	}
}

func TestRollbackRetryVerifiesArchiveInsteadOfRecapturingRestoredData(t *testing.T) {
	txn, _, _ := rollbackFixture(t)
	ctx := context.Background()
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txn.path("/var/lib/payesh/payesh.db"), []byte("candidate-data"), 0600); err != nil {
		t.Fatal(err)
	}
	service := txn.Services
	txn.Services = func(ctx context.Context, action, role, init string) error {
		if action == "reload" {
			return errors.New("interrupted restore")
		}
		return service(ctx, action, role, init)
	}
	if err := txn.Rollback(ctx); err == nil {
		t.Fatal("injected restore interruption ignored")
	}
	archive := filepath.Join(txn.dir(), "failed-generation", "payesh.db")
	if err := os.Chmod(archive, 0666); err != nil {
		t.Fatal(err)
	}
	txn.Services = service
	if _, err := txn.Recover(ctx); err == nil {
		t.Fatal("writable recovery evidence accepted")
	}
	if err := os.Chmod(archive, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	txn.Services = service
	if _, err := txn.Recover(ctx); err == nil {
		t.Fatal("damaged recovery evidence accepted")
	}
	if err := os.WriteFile(archive, []byte("candidate-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if recovered, err := txn.Recover(ctx); err != nil || !recovered {
		t.Fatalf("resume: %v %v", recovered, err)
	}
	got, err := os.ReadFile(archive)
	if err != nil || string(got) != "candidate-data" {
		t.Fatalf("archive replaced with old generation: %q %v", got, err)
	}
}

func TestRollbackRestartRetryRetainsSamplesWrittenByPartiallyStartedServices(t *testing.T) {
	txn, _, calls := rollbackFixture(t)
	ctx := context.Background()
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txn.path("/var/lib/payesh/payesh.db"), []byte("candidate-data"), 0600); err != nil {
		t.Fatal(err)
	}
	service := txn.Services
	firstStart := true
	txn.Services = func(ctx context.Context, action, role, init string) error {
		if err := service(ctx, action, role, init); err != nil {
			return err
		}
		if action != "start" || !firstStart {
			return nil
		}
		if phase, err := txn.Phase(); err != nil || phase != "restarting" {
			t.Fatalf("service started before durable restoration boundary: phase=%q err=%v", phase, err)
		}
		firstStart = false
		db, err := sql.Open("sqlite", "file:"+txn.path("/var/lib/payesh/payesh.db"))
		if err != nil {
			return err
		}
		_, err = db.Exec("INSERT INTO snapshot VALUES('sample-after-restore')")
		closeErr := db.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return errors.New("second service failed after first service accepted a sample")
	}
	if err := txn.Rollback(ctx); err == nil {
		t.Fatal("injected partial start failure ignored")
	}
	if recovered, err := txn.Recover(ctx); err != nil || !recovered {
		t.Fatalf("restart retry: %v %v", recovered, err)
	}
	db, err := sql.Open("sqlite", "file:"+txn.path("/var/lib/payesh/payesh.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var samples int
	if err := db.QueryRow("SELECT COUNT(*) FROM snapshot WHERE value='sample-after-restore'").Scan(&samples); err != nil || samples != 1 {
		t.Fatalf("retry discarded a sample accepted by restored services: count=%d err=%v", samples, err)
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "stop", "reload", "start", "reload", "start"}) {
		t.Fatalf("retry stopped or re-restored a running generation: %v", *calls)
	}
	archive, err := os.ReadFile(filepath.Join(txn.dir(), "failed-generation", "payesh.db"))
	if err != nil || string(archive) != "candidate-data" {
		t.Fatalf("retry changed candidate evidence: %q %v", archive, err)
	}
}

func TestRollbackInsufficientPreservationHeadroomLeavesCandidateUntouched(t *testing.T) {
	txn, _, _ := rollbackFixture(t)
	ctx := context.Background()
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txn.path("/var/lib/payesh/payesh.db"), []byte("candidate-data"), 0600); err != nil {
		t.Fatal(err)
	}
	txn.AvailableBytes = func(string) (uint64, error) { return 0, nil }
	if err := txn.Rollback(ctx); err == nil {
		t.Fatal("restore ignored preservation headroom")
	}
	got, err := os.ReadFile(txn.path("/var/lib/payesh/payesh.db"))
	if err != nil || string(got) != "candidate-data" {
		t.Fatalf("candidate lost under disk pressure: %q %v", got, err)
	}
}

func TestRollbackArchiveRecoversCandidateSampleCommittedOnlyToWAL(t *testing.T) {
	txn, _, _ := rollbackFixture(t)
	ctx := context.Background()
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	// Generate a quiescent raw database/WAL pair elsewhere. Closing this writer
	// cannot checkpoint or remove the copied candidate files under test.
	producer := filepath.Join(t.TempDir(), "producer.db")
	db, err := sql.Open("sqlite", "file:"+producer)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE evidence(value TEXT); PRAGMA wal_checkpoint(TRUNCATE); INSERT INTO evidence VALUES('wal-only-candidate-sample')`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(producer + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if suffix == "-wal" && len(data) <= 32 {
			t.Fatal("fixture has no committed WAL frames")
		}
		if err := os.WriteFile(txn.path("/var/lib/payesh/payesh.db"+suffix), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(t.TempDir(), "without-wal.db")
	body, err := os.ReadFile(txn.path("/var/lib/payesh/payesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, body, 0600); err != nil {
		t.Fatal(err)
	}
	withoutWAL, err := sql.Open("sqlite", "file:"+plain+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var checkpointed int
	err = withoutWAL.QueryRow(`SELECT COUNT(*) FROM evidence`).Scan(&checkpointed)
	closeErr := withoutWAL.Close()
	if err != nil || closeErr != nil || checkpointed != 0 {
		t.Fatalf("sample was not WAL-only: count=%d err=%v close=%v", checkpointed, err, closeErr)
	}
	if err := txn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(txn.dir(), "failed-generation")
	replay := filepath.Join(t.TempDir(), "replay.db")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(filepath.Join(archive, "payesh.db"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replay+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := sql.Open("sqlite", "file:"+replay+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	var value string
	if err := recovered.QueryRow(`SELECT value FROM evidence`).Scan(&value); err != nil || value != "wal-only-candidate-sample" {
		t.Fatalf("archived WAL sample=%q err=%v", value, err)
	}
	// Inspection uses a copy; the retained evidence still passes verification.
	if err := txn.verifyFailedGeneration(ctx, archive); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledLayoutRecoversInterruptedActivationAndKeepsCommittedVersion(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted", true: "committed"}[commit], func(t *testing.T) {
			txn, _, _ := rollbackFixture(t)
			ctx := context.Background()
			if err := txn.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if err := txn.MarkActivating(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(txn.path("/usr/bin/payesh"), []byte("new-version"), 0755); err != nil {
				t.Fatal(err)
			}
			if commit {
				if err := txn.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			restarted := txn
			restored, err := restarted.Recover(ctx)
			if err != nil || restored == commit {
				t.Fatalf("restored=%v err=%v", restored, err)
			}
			got, _ := os.ReadFile(txn.path("/usr/bin/payesh"))
			want := "old-version"
			if commit {
				want = "new-version"
			}
			if string(got) != want {
				t.Fatalf("version=%q", got)
			}
		})
	}
}

func TestProtectedScopeAndSnapshotRefuseSymlinksOrWritableScope(t *testing.T) {
	txn, _, _ := rollbackFixture(t)
	path := scopePath(txn.Root)
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstallationScope(txn.Root); err == nil {
		t.Fatal("service-writable scope accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte(`{"role":"hub","init":"systemd"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstallationScope(txn.Root); err == nil {
		t.Fatal("symlink scope accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteInstallationScope(txn.Root, InstallationScope{Role: "node", Init: "systemd"}); err != nil {
		t.Fatal(err)
	}
	binary := txn.path("/usr/bin/payesh")
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, binary); err != nil {
		t.Fatal(err)
	}
	if err := txn.Prepare(context.Background()); err == nil {
		t.Fatal("symlink artifact snapshotted")
	}
}

func TestPreparePartialStopFailureRestartsPreviousServices(t *testing.T) {
	txn, _, calls := rollbackFixture(t)
	txn.Services = func(_ context.Context, action, role, init string) error {
		*calls = append(*calls, action)
		if action == "stop" {
			return errors.New("second service failed after first stopped")
		}
		return nil
	}
	if err := txn.Prepare(context.Background()); err == nil {
		t.Fatal("stop failure ignored")
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "start"}) {
		t.Fatalf("partially stopped installation was not restarted: %v", *calls)
	}
	if phase, err := txn.Phase(); err != nil || phase != "rolled-back" {
		t.Fatalf("failed snapshot phase=%q err=%v", phase, err)
	}
}

func TestCachedFleetSuccessStillRequiresCurrentHealth(t *testing.T) {
	dir := t.TempDir()
	req := Request{JobID: "cached-health-fleet01", Version: "1.2.0", Deadline: time.Now().Add(time.Hour)}
	if err := SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFleet(context.Background(), dir, req, "1.2.0", func(context.Context, string) error { return nil }, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("restarted version is unhealthy")
	if done, err := RunFleet(context.Background(), dir, req, "1.2.0", func(context.Context, string) error { t.Fatal("cached success reinstalled"); return nil }, func(context.Context, string) error { return cause }); done || !errors.Is(err, cause) {
		t.Fatalf("cached success bypassed health: done=%v err=%v", done, err)
	}
	result, err := FleetResult(dir, req.JobID)
	if err != nil || result.State == StateSucceeded {
		t.Fatalf("unhealthy cached completion still reports success: result=%+v err=%v", result, err)
	}
}

func TestHealthFailureRestoresInstallationAndPreservesOriginalFailure(t *testing.T) {
	txn, contents, _ := rollbackFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	req := Request{JobID: txn.JobID, Version: "1.2.0", Deadline: time.Now().Add(time.Hour)}
	if err := SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("candidate service health failed")
	done, err := RunFleet(ctx, dir, req, "1.0.0", func(ctx context.Context, _ string) error {
		if err := txn.Prepare(ctx); err != nil {
			return err
		}
		if err := txn.MarkActivating(); err != nil {
			return err
		}
		return os.WriteFile(txn.path("/usr/bin/payesh"), []byte("new-version"), 0755)
	}, func(ctx context.Context, _ string) error {
		if err := txn.Rollback(ctx); err != nil {
			return err
		}
		return cause
	})
	if done || !errors.Is(err, cause) {
		t.Fatalf("health failure lost: done=%v err=%v", done, err)
	}
	for path, want := range contents {
		got, readErr := os.ReadFile(txn.path(path))
		if readErr != nil || string(got) != want {
			t.Fatalf("path=%s got=%q err=%v", path, got, readErr)
		}
	}
	result, err := FleetResult(dir, req.JobID)
	if err != nil || result.State != StateFailed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommittedActivationSurvivesResultPublicationGap(t *testing.T) {
	txn, _, _ := rollbackFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	req := Request{JobID: txn.JobID, Version: "1.2.0", Deadline: time.Now().Add(time.Hour)}
	if err := SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txn.path("/usr/bin/payesh"), []byte("1.2.0"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
	restored, err := txn.Recover(ctx)
	if err != nil || restored {
		t.Fatalf("committed activation restored=%v err=%v", restored, err)
	}
	done, err := RunFleet(ctx, dir, req, "1.2.0", func(context.Context, string) error { t.Fatal("committed activation was repeated"); return nil }, func(context.Context, string) error { return nil })
	if err != nil || !done {
		t.Fatalf("completion recovery done=%v err=%v", done, err)
	}
}

func TestRollbackRemovesCandidateDatabaseSidecarsWhenNoPreviousDatabase(t *testing.T) {
	txn, _, _ := rollbackFixture(t)
	ctx := context.Background()
	db := txn.path("/var/lib/payesh/payesh.db")
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	if err := txn.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := txn.MarkActivating(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(db+suffix, []byte("candidate"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := txn.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(db + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate %s remains: %v", suffix, err)
		}
	}
}

func TestSnapshotLimitsRejectBeforeStoppingServices(t *testing.T) {
	for _, kind := range []string{"oversize", "headroom", "archive budget"} {
		t.Run(kind, func(t *testing.T) {
			txn, _, calls := rollbackFixture(t)
			switch kind {
			case "oversize":
				txn.SnapshotLimitBytes = 1
			case "headroom":
				txn.AvailableBytes = func(string) (uint64, error) { return 1, nil }
			case "archive budget":
				txn.StorageBudgetBytes = 1
			}
			if err := txn.Prepare(context.Background()); err == nil {
				t.Fatal("snapshot storage gate ignored")
			}
			if len(*calls) != 0 {
				t.Fatalf("services stopped before storage rejection: %v", *calls)
			}
		})
	}
}

func TestSnapshotConcurrentGrowthIsBoundedAndRestartsServices(t *testing.T) {
	txn, _, calls := rollbackFixture(t)
	txn.SnapshotLimitBytes = 128 * 1024
	txn.Services = func(_ context.Context, action, role, init string) error {
		*calls = append(*calls, action)
		if action == "stop" {
			return os.WriteFile(txn.path("/usr/bin/payesh"), []byte(strings.Repeat("x", 256*1024)), 0640)
		}
		return nil
	}
	if err := txn.Prepare(context.Background()); err == nil {
		t.Fatal("concurrently grown snapshot exceeded bound without failure")
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "start"}) {
		t.Fatalf("snapshot copyfailure did not restart old services: %v", *calls)
	}
	if _, err := os.Lstat(filepath.Join(txn.dir(), "files")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed capture still consumes snapshot storage: %v", err)
	}
}

func TestSnapshotStorageBudgetNeverDeletesRetainedArchives(t *testing.T) {
	txn, _, calls := rollbackFixture(t)
	txn.StorageBudgetBytes = 80 << 20
	retained := filepath.Join(filepath.Dir(txn.dir()), "unresolved-other-job")
	if err := os.MkdirAll(retained, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(retained, "snapshot")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(20 << 20); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = txn.Prepare(context.Background()); err == nil {
		t.Fatal("retained snapshot pressure was ignored")
	}
	if len(*calls) != 0 {
		t.Fatalf("storage budget stopped services: %v", *calls)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 20<<20 {
		t.Fatalf("existing archive was removed: %v", err)
	}
}

func TestSnapshotDirectorySwapCannotReadOutsideManagedTree(t *testing.T) {
	txn, _, calls := rollbackFixture(t)
	managed := txn.path("/etc/payesh/subdir")
	outside := filepath.Join(txn.Root, "outside-managed-scope")
	for _, path := range []string{managed, outside} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(managed, "inside"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	swapped := false
	txn.beforeSnapshotOpen = func(path string) {
		if path != managed || swapped {
			return
		}
		swapped = true
		if err := os.Rename(managed, managed+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, managed); err != nil {
			t.Fatal(err)
		}
	}
	if err := txn.Prepare(context.Background()); err == nil {
		t.Fatal("directory replaced by external symlink was captured")
	}
	if !swapped {
		t.Fatal("directory swap boundary was not exercised")
	}
	if !reflect.DeepEqual(*calls, []string{"stop", "start"}) {
		t.Fatalf("directory swap lost previous services: %v", *calls)
	}
	if _, err := os.Lstat(filepath.Join(txn.dir(), "files")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe capture payload remains: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(data) != "outside-secret" {
		t.Fatalf("outside data modified: %s %v", data, err)
	}
}
