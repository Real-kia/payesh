package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/webupdate"
)

func TestFleetWorkerRejectsPreviewAndMissingAnchorDurably(t *testing.T) {
	for _, mode := range []string{"preview", "production"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PAYESH_RELEASE_MODE", mode)
			t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", "")
			t.Setenv("PAYESH_RELEASE_KEY_ID", "")
			dir := t.TempDir()
			req := webupdate.Request{JobID: "test-fleet-job", Version: "999.0.0", Deadline: time.Now().Add(time.Minute)}
			if err := webupdate.SubmitFleet(dir, req, time.Now()); err != nil {
				t.Fatal(err)
			}
			done, err := runFleetUpdateWithRecovery(context.Background(), dir, req,
				func(context.Context, webupdate.InstallationTransaction) (bool, error) {
					t.Fatal("missing trust performed recovery")
					return false, nil
				},
				func(webupdate.InstallationTransaction) (string, error) { return "", os.ErrNotExist })
			if err == nil || done {
				t.Fatalf("unsafe mode accepted: done=%v error=%v", done, err)
			}
			result, err := webupdate.FleetResult(dir, req.JobID)
			if err != nil || result.State != webupdate.StateFailed {
				t.Fatalf("missing durable failure: %+v %v", result, err)
			}
			_, found, err := webupdate.Take(dir)
			if err != nil || found {
				t.Fatalf("completed failure request retained: %v %v", found, err)
			}
		})
	}
}

func TestFleetHealthUsesFreshExecutableAndRoleServices(t *testing.T) {
	for _, init := range []string{"systemd", "openrc"} {
		for _, role := range []string{"node", "hub", "cli-only"} {
			t.Run(init+"/"+role, func(t *testing.T) {
				state := filepath.Join(t.TempDir(), "install-state.json")
				os.WriteFile(state, []byte(`{"role":"`+role+`"}`), 0o600)
				var calls []string
				run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
					calls = append(calls, name+" "+strings.Join(args, " "))
					if name == "/usr/bin/payesh" {
						return []byte("1.2.3\n"), nil
					}
					return nil, nil
				}
				if err := verifyFleetReleaseHealth(context.Background(), "1.2.3", state, init, run); err != nil {
					t.Fatal(err)
				}
				want := 1
				if role == "node" {
					want = 2
				}
				if role == "hub" {
					want = 3
				}
				if len(calls) != want {
					t.Fatalf("unexpected checks %v", calls)
				}
				if role == "node" && !strings.Contains(calls[1], "payesh-agent") {
					t.Fatal("agent health omitted")
				}
				if err := verifyFleetReleaseHealth(context.Background(), "1.2.4", state, init, run); err == nil {
					t.Fatal("wrong fresh executable version accepted")
				}
			})
		}
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "install-state.json")
	os.WriteFile(state, []byte(`{"role":"node"}`), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := verifyFleetReleaseHealth(ctx, "1.2.3", state, "systemd", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/payesh" {
			return []byte("1.2.3"), nil
		}
		return nil, errors.New("inactive")
	})
	if err == nil {
		t.Fatal("inactive service accepted")
	}
}

func TestFleetWorkerRecoveryRetainsIntentUntilComplete(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(key, pub, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", key)
	t.Setenv("PAYESH_RELEASE_KEY_ID", "test-release")
	for _, tc := range []struct {
		name                           string
		expired, recovered, incomplete bool
		wantErr                        error
		wantRollback                   string
	}{
		{"expired-incomplete", true, true, true, webupdate.ErrRollbackIncomplete, ""},
		{"authorized-incomplete", false, true, true, webupdate.ErrRollbackIncomplete, ""},
		{"expired-fresh", true, false, false, nil, ""},
		{"authorized-restored", false, true, false, webupdate.ErrInstallationRestored, "restored"},
		{"expired-restored", true, true, false, webupdate.ErrInstallationRestored, "restored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			deadline := time.Now().Add(time.Minute)
			if tc.expired {
				deadline = time.Now().Add(-time.Minute)
			}
			req := webupdate.Request{JobID: "recovery-test", Version: "999.0.0", Deadline: deadline}
			if err := webupdate.SubmitFleet(dir, req, req.Deadline.Add(-2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(dir, webupdate.RequestFile))
			if err != nil {
				t.Fatal(err)
			}
			done, err := runFleetUpdateWithRecovery(context.Background(), dir, req, func(context.Context, webupdate.InstallationTransaction) (bool, error) {
				if tc.incomplete {
					return tc.recovered, fmt.Errorf("archive verification: %w", errors.New("archive corrupted"))
				}
				return tc.recovered, nil
			}, func(txn webupdate.InstallationTransaction) (string, error) { return txn.Phase() })
			if done || err == nil {
				t.Fatalf("unexpected completion: done=%v err=%v", done, err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("recovery error lost: %v", err)
			}
			if tc.name == "expired-fresh" && !strings.Contains(err.Error(), "authorization expired") {
				t.Fatalf("expired authorization reached activation: %v", err)
			}
			if tc.incomplete {
				retained, err := os.ReadFile(filepath.Join(dir, webupdate.RequestFile))
				if err != nil || string(retained) != string(original) {
					t.Fatalf("recovery intent changed or removed: %q %v", retained, err)
				}
				if result, err := webupdate.FleetResult(dir, req.JobID); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unresolved recovery published terminal result: %+v %v", result, err)
				}
			} else {
				if _, err := os.Stat(filepath.Join(dir, webupdate.RequestFile)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("completed failure retained request: %v", err)
				}
				result, err := webupdate.FleetResult(dir, req.JobID)
				if err != nil || result.State != webupdate.StateFailed || result.Rollback != tc.wantRollback {
					t.Fatalf("incorrect completion: %+v %v", result, err)
				}
			}
		})
	}
}

func TestFleetWorkerMissingTrustRetainsUnresolvedRecovery(t *testing.T) {
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", "")
	t.Setenv("PAYESH_RELEASE_KEY_ID", "")
	for _, tc := range []struct {
		phase    string
		phaseErr error
		retain   bool
	}{
		{"capturing", nil, true}, {"prepared", nil, true}, {"activating", nil, true},
		{"preserving", nil, true}, {"restoring", nil, true}, {"restarting", nil, true},
		{"unknown", nil, true}, {"", errors.New("invalid rollback journal"), true},
		{"", os.ErrNotExist, false}, {"committed", nil, false}, {"rolled-back", nil, false},
	} {
		t.Run(tc.phase+fmt.Sprint(tc.phaseErr), func(t *testing.T) {
			dir := t.TempDir()
			req := webupdate.Request{JobID: "missing-trust-recovery", Version: "999.0.0", Deadline: time.Now().Add(-time.Minute)}
			if err := webupdate.SubmitFleet(dir, req, req.Deadline.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(dir, webupdate.RequestFile))
			if err != nil {
				t.Fatal(err)
			}
			done, err := runFleetUpdateWithRecovery(context.Background(), dir, req,
				func(context.Context, webupdate.InstallationTransaction) (bool, error) {
					t.Fatal("missing trust performed recovery service actions")
					return false, nil
				},
				func(webupdate.InstallationTransaction) (string, error) { return tc.phase, tc.phaseErr })
			if done || err == nil {
				t.Fatalf("missing trust accepted: done=%v err=%v", done, err)
			}
			errCompletion := err
			if tc.retain {
				if !errors.Is(err, webupdate.ErrRollbackIncomplete) {
					t.Fatalf("unresolved journal terminalized: %v", err)
				}
				retained, err := os.ReadFile(filepath.Join(dir, webupdate.RequestFile))
				if err != nil || string(retained) != string(original) {
					t.Fatalf("recovery intent changed or removed: %q %v", retained, err)
				}
				if result, err := webupdate.FleetResult(dir, req.JobID); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unresolved recovery published terminal result: %+v %v", result, err)
				}
			} else {
				if _, err := os.Stat(filepath.Join(dir, webupdate.RequestFile)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("resolved failure retained request: %v", err)
				}
				result, err := webupdate.FleetResult(dir, req.JobID)
				if err != nil || result.State != webupdate.StateFailed {
					t.Fatalf("missing durable policy failure: %+v %v", result, err)
				}
				if tc.phase == "rolled-back" && (!errors.Is(errCompletion, webupdate.ErrInstallationRestored) || result.Rollback != "restored") {
					t.Fatalf("restored generation lost completion marker: %+v %v", result, errCompletion)
				}
			}
		})
	}
}

func TestFleetCandidateSchemaRefusalPrecedesSnapshotAndServiceStop(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(key, pub, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", key)
	t.Setenv("PAYESH_RELEASE_KEY_ID", "test-release")
	dir := t.TempDir()
	req := webupdate.Request{JobID: "schema-preflight", Version: "999.0.0", Deadline: time.Now().Add(time.Minute)}
	if err := webupdate.SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	checked, prepared := false, false
	refusal := errors.New("candidate cannot read installed schema")
	done, err := runFleetUpdateWithHooks(context.Background(), dir, req,
		func(context.Context, webupdate.InstallationTransaction) (bool, error) { return false, nil },
		func(webupdate.InstallationTransaction) (string, error) { return "", os.ErrNotExist },
		fleetUpdateHooks{Scope: func() (webupdate.InstallationScope, error) {
			return webupdate.InstallationScope{Role: "hub", Init: "openrc"}, nil
		}, CandidatePreflight: func(context.Context, string) error { checked = true; return refusal }, Prepare: func(context.Context) error { prepared = true; return errors.New("unexpected snapshot boundary") }})
	if done || !errors.Is(err, refusal) || !checked || prepared {
		t.Fatalf("candidate refusal reached snapshot/service boundary: checked=%v prepared=%v done=%v err=%v", checked, prepared, done, err)
	}
	result, err := webupdate.FleetResult(dir, req.JobID)
	if err != nil || result.State != webupdate.StateFailed {
		t.Fatalf("missing durable refusal: %+v %v", result, err)
	}
}

func TestFleetAuthorizationExpiryDuringCandidatePreflightNeverPrepares(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(key, pub, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", key)
	t.Setenv("PAYESH_RELEASE_KEY_ID", "test-release")
	dir := t.TempDir()
	req := webupdate.Request{JobID: "schema-preflight-expiry", Version: "999.0.0", Deadline: time.Now().Add(200 * time.Millisecond)}
	if err := webupdate.SubmitFleet(dir, req, time.Now()); err != nil {
		t.Fatal(err)
	}
	checked, prepared := false, false
	_, err = runFleetUpdateWithHooks(t.Context(), dir, req, func(context.Context, webupdate.InstallationTransaction) (bool, error) { return false, nil }, func(webupdate.InstallationTransaction) (string, error) { return "", os.ErrNotExist }, fleetUpdateHooks{Scope: func() (webupdate.InstallationScope, error) {
		return webupdate.InstallationScope{Role: "hub", Init: "openrc"}, nil
	}, CandidatePreflight: func(ctx context.Context, _ string) error { checked = true; <-ctx.Done(); return nil }, Prepare: func(context.Context) error { prepared = true; return errors.New("expired snapshot boundary") }})
	if !checked || prepared || err == nil {
		t.Fatalf("expired preflight crossed snapshot boundary: checked=%v prepared=%v err=%v", checked, prepared, err)
	}
}
