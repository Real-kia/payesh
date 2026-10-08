package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/webupdate"
)

func TestInstalledRoleRejectsUnknownAndReadsNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-state.json")
	for _, role := range []string{"node", "unknown"} {
		data, _ := json.Marshal(map[string]string{"role": role})
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := installedRole(path)
		if role == "node" && (err != nil || got != role) {
			t.Fatalf("role=%q err=%v", got, err)
		}
		if role == "unknown" && err == nil {
			t.Fatal("unknown role accepted")
		}
	}
}

func TestBrowserWorkerIntentRequiresConfiguredTrustWhenModeIsProduction(t *testing.T) {
	t.Setenv("PAYESH_RELEASE_MODE", "production")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", "")
	t.Setenv("PAYESH_RELEASE_KEY_ID", "")
	dir := t.TempDir()
	if err := webupdate.Submit(dir, webupdate.Request{Version: "1.2.4"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	req, found, err := webupdate.Take(dir)
	if err != nil || !found || req.JobID == "" {
		t.Fatalf("browser intent=%+v %v %v", req, found, err)
	}
	recoveryCalled := false
	done, err := runFleetUpdateWithRecovery(context.Background(), dir, req, func(context.Context, webupdate.InstallationTransaction) (bool, error) {
		recoveryCalled = true
		return false, nil
	}, func(webupdate.InstallationTransaction) (string, error) { return "", os.ErrNotExist })
	if err == nil || done || recoveryCalled {
		t.Fatalf("missing trust worker activation=%v %v recovery=%v", done, err, recoveryCalled)
	}
	result, err := webupdate.FleetResult(dir, req.JobID)
	if err != nil || result.State != webupdate.StateFailed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestBrowserWorkerIntentAllowsPreview(t *testing.T) {
	t.Setenv("PAYESH_RELEASE_MODE", "preview")
	t.Setenv("PAYESH_RELEASE_PUBLIC_KEY", "")
	t.Setenv("PAYESH_RELEASE_KEY_ID", "")
	dir := t.TempDir()
	if err := webupdate.Submit(dir, webupdate.Request{Version: "1.2.4"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	req, found, err := webupdate.Take(dir)
	if err != nil || !found || req.JobID == "" {
		t.Fatalf("browser intent=%+v %v %v", req, found, err)
	}
	preflightChecked := false
	testErr := errors.New("preflight reached")
	_, err = runFleetUpdateWithHooks(context.Background(), dir, req,
		func(context.Context, webupdate.InstallationTransaction) (bool, error) { return false, nil },
		func(webupdate.InstallationTransaction) (string, error) { return "", os.ErrNotExist },
		fleetUpdateHooks{
			Scope: func() (webupdate.InstallationScope, error) {
				return webupdate.InstallationScope{Role: "standalone", Init: "openrc"}, nil
			},
			CandidatePreflight: func(context.Context, string) error {
				preflightChecked = true
				return testErr
			},
		})
	if !preflightChecked {
		t.Fatalf("expected preview worker to reach preflight check, got err: %v", err)
	}
}
