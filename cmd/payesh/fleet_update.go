package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/version"
	"github.com/Real-kia/payesh/internal/webupdate"
)

func runFleetUpdate(ctx context.Context, dir string, req webupdate.Request) (bool, error) {
	return runFleetUpdateWithRecovery(ctx, dir, req, func(ctx context.Context, txn webupdate.InstallationTransaction) (bool, error) {
		return txn.Recover(ctx)
	}, func(txn webupdate.InstallationTransaction) (string, error) {
		return txn.Phase()
	})
}

func runFleetUpdateWithRecovery(ctx context.Context, dir string, req webupdate.Request, recoverInstallation func(context.Context, webupdate.InstallationTransaction) (bool, error), installationPhase func(webupdate.InstallationTransaction) (string, error)) (bool, error) {
	return runFleetUpdateWithHooks(ctx, dir, req, recoverInstallation, installationPhase, fleetUpdateHooks{})
}

// Hooks expose the activation boundaries for disposable worker tests.
// Production uses only root-owned installation paths and authenticated release data.
type fleetUpdateHooks struct {
	Scope              func() (webupdate.InstallationScope, error)
	Prepare            func(context.Context) error
	CandidatePreflight func(context.Context, string) error
}

func runFleetUpdateWithHooks(ctx context.Context, dir string, req webupdate.Request, recoverInstallation func(context.Context, webupdate.InstallationTransaction) (bool, error), installationPhase func(webupdate.InstallationTransaction) (string, error), hooks fleetUpdateHooks) (bool, error) {
	production := func() error {
		policy, err := releaseTrustFromEnvironment()
		if err != nil {
			return err
		}
		if policy.mode != "production" {
			return errors.New("remote fleet updates require production release authentication; preview mode refused")
		}
		return nil
	}
	txn := webupdate.InstallationTransaction{Root: "/", JobID: req.JobID, Services: func(ctx context.Context, action, role, init string) error {
		var services []string
		if role == "node" {
			services = []string{"payesh-agent"}
		} else if role == "hub" || role == "standalone" {
			services = []string{"payesh-agent", "payesh-server"}
		} else {
			return errors.New("invalid rollback service role")
		}
		if action == "reload" {
			if init == "systemd" {
				return exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
			}
			return nil
		}
		if action != "stop" && action != "start" {
			return errors.New("invalid rollback service action")
		}
		for _, service := range services {
			var err error
			if init == "systemd" {
				err = exec.CommandContext(ctx, "systemctl", action, service).Run()
			} else if init == "openrc" {
				err = exec.CommandContext(ctx, "rc-service", service, action).Run()
			} else {
				return errors.New("invalid rollback init system")
			}
			if err != nil {
				return err
			}
		}
		return nil
	}}
	// Missing trust authorizes neither activation nor recovery service actions.
	// Preserve unresolved or unreadable journals so provisioning repair can
	// resume recovery instead of losing the only retry intent.
	if policyErr := production(); policyErr != nil {
		phase, phaseErr := installationPhase(txn)
		if (phaseErr != nil && !errors.Is(phaseErr, os.ErrNotExist)) || (phaseErr == nil && phase != "committed" && phase != "rolled-back") {
			return false, webupdate.InstallationRollbackError(policyErr, errors.New("recovery requires a valid release trust configuration"))
		}
		if phaseErr == nil && phase == "rolled-back" {
			return webupdate.CompleteFleetRollback(dir, req)
		}
		refuse := func(context.Context, string) error { return policyErr }
		return webupdate.RunFleet(ctx, dir, req, version.Value, refuse, refuse)
	}
	rolledBack, recoveryErr := recoverInstallation(ctx, txn)
	if recoveryErr != nil {
		// Recovery must finish before authorization expiry or replay handling
		// can publish a terminal result and remove the retry intent.
		return false, webupdate.InstallationRollbackError(errors.New("interrupted activation recovery"), recoveryErr)
	}
	if rolledBack {
		return webupdate.CompleteFleetRollback(dir, req)
	}
	rollback := func(cause error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := txn.Rollback(restoreCtx); err != nil {
			return webupdate.InstallationRollbackError(cause, err)
		}
		return webupdate.InstallationRollbackError(cause, nil)
	}
	install := func(ctx context.Context, target string) error {
		if err := production(); err != nil {
			return err
		}
		readScope := hooks.Scope
		if readScope == nil {
			readScope = func() (webupdate.InstallationScope, error) { return webupdate.ReadInstallationScope("/") }
		}
		scope, err := readScope()
		if err != nil {
			return err
		}
		var preparedRelease preparedProductionRelease
		var preparedPolicy releaseTrustPolicy
		if hooks.CandidatePreflight != nil {
			if err := hooks.CandidatePreflight(ctx, target); err != nil {
				return err
			}
		} else {
			preparedPolicy, err = releaseTrustFromEnvironment()
			if err != nil {
				return err
			}
			preparedRelease, err = prepareProductionRelease(ctx, &http.Client{Timeout: 15 * time.Second}, os.Getenv("GITHUB_TOKEN"), target, version.Value, preparedPolicy, "/var/lib/payesh/payesh.db")
			if err != nil {
				return err
			}
		}
		// Authorization can expire while authenticated metadata is fetched.
		// Recovery already ran above; fresh activation must still be authorized.
		if err := ctx.Err(); err != nil {
			return err
		}
		if !req.Deadline.After(time.Now().UTC()) {
			return errors.New("fleet update authorization expired during candidate preflight")
		}
		prepare := hooks.Prepare
		if prepare == nil {
			prepare = txn.Prepare
		}
		if err := prepare(ctx); err != nil {
			return err
		}
		if err := txn.MarkActivating(); err != nil {
			return rollback(err)
		}
		if err := runPreparedReleaseInstaller(ctx, target, preparedPolicy, preparedRelease, "--role", scope.Role, "--version", target); err != nil {
			return rollback(err)
		}
		return nil
	}
	health := func(ctx context.Context, target string) error {
		if err := production(); err != nil {
			return err
		}
		init := "openrc"
		if info, err := os.Stat("/run/systemd/system"); err == nil && info.IsDir() {
			init = "systemd"
		}
		if err := verifyFleetReleaseHealth(ctx, target, webupdate.InstallationScopePath, init, func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}); err != nil {
			if phase, _ := txn.Phase(); phase == "activating" {
				return rollback(err)
			}
			return err
		}
		if phase, err := txn.Phase(); err == nil && phase == "activating" {
			return txn.Commit()
		}
		return nil
	}
	return webupdate.RunFleet(ctx, dir, req, version.Value, install, health)
}

type fleetHealthCommand func(context.Context, string, ...string) ([]byte, error)

// Health is checked using the newly installed executable rather than the old
// worker's compiled version. Only services required by the installed role count.
func verifyFleetReleaseHealth(ctx context.Context, target, statePath, init string, run fleetHealthCommand) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	role, err := installedRole(statePath)
	if statePath == webupdate.InstallationScopePath {
		scope, scopeErr := webupdate.ReadInstallationScope("/")
		if scopeErr != nil {
			return scopeErr
		}
		role = scope.Role
		init = scope.Init
	}
	if err != nil {
		return err
	}
	out, err := run(ctx, "/usr/bin/payesh", "version")
	if err != nil || strings.TrimSpace(string(out)) != target {
		return errors.New("installed Payesh version does not match fleet update target")
	}
	var services []string
	switch role {
	case "node":
		services = []string{"payesh-agent"}
	case "hub", "standalone":
		services = []string{"payesh-agent", "payesh-server"}
	}
	for _, service := range services {
		for {
			var err error
			if init == "systemd" {
				_, err = run(ctx, "systemctl", "is-active", "--quiet", service)
			} else if init == "openrc" {
				_, err = run(ctx, "rc-service", service, "status")
			} else {
				return errors.New("unsupported init system for fleet health check")
			}
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("fleet update health check failed for %s", service)
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}
