package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/updater"
	"github.com/Real-kia/payesh/internal/version"
	"github.com/Real-kia/payesh/internal/webupdate"
)

func updateCommand(ctx context.Context, args []string) error {
	check := false
	if len(args) > 0 && args[0] == "check" {
		check = true
		args = args[1:]
	}
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	checkFlag := flags.Bool("check", false, "check GitHub without installing")
	requested := flags.String("version", "", "install a specific release version")
	jsonOutput := flags.Bool("json", false, "print check result as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("usage: payesh update [check] [--version X.Y.Z]")
	}
	check = check || *checkFlag
	if *requested != "" && !updater.ValidRelease(strings.TrimPrefix(*requested, "v")) {
		return errors.New("invalid release version")
	}
	if runtime.GOOS != "linux" && !check {
		return errors.New("updates require Linux")
	}
	token := os.Getenv("GITHUB_TOKEN")
	latest, err := (release.GitHubClient{Token: token}).Latest(ctx)
	if err != nil {
		return err
	}
	target := latest.Version
	if *requested != "" {
		target = strings.TrimPrefix(*requested, "v")
	}
	comparison, comparable := 0, false
	if updater.ValidRelease(version.Value) {
		comparison, err = updater.CompareReleases(version.Value, target)
		if err != nil {
			return err
		}
		comparable = true
	}
	available := !comparable || comparison < 0
	if check {
		status := struct {
			Current         string `json:"current"`
			Latest          string `json:"latest"`
			UpdateAvailable bool   `json:"update_available"`
			URL             string `json:"url"`
		}{version.Value, latest.Version, available, latest.URL}
		if *jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(status)
		}
		fmt.Printf("Current: %s\nLatest: %s\nUpdate available: %t\nRelease: %s\n", status.Current, status.Latest, status.UpdateAvailable, status.URL)
		return nil
	}
	if *jsonOutput {
		return errors.New("--json is only supported with check")
	}
	if os.Geteuid() != 0 {
		return errors.New("run the update as root: sudo payesh update (for a private repo, preserve GITHUB_TOKEN)")
	}
	return installRelease(ctx, target, token, comparable, comparison)
}

// installRelease runs the release installer for target. comparison is the
// result of comparing the installed version with target when comparable.
func installRelease(ctx context.Context, target, token string, comparable bool, comparison int) error {
	if comparable && comparison == 0 {
		fmt.Printf("Payesh %s is already installed.\n", version.Value)
		return nil
	}
	if comparable && comparison > 0 {
		return fmt.Errorf("installed version %s is newer than requested %s; downgrade refused", version.Value, target)
	}
	role, err := installedRole("/var/lib/payesh/install-state.json")
	if err != nil {
		return err
	}
	return runReleaseInstaller(ctx, target, token, "--role", role, "--version", target)
}

// updateWorker is the root service that applies updates requested from the
// web UI. It accepts only a validated release version from the request file.
func updateWorker(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("update-worker", flag.ContinueOnError)
	dir := flags.String("dir", webupdate.DefaultDir, "directory holding update request and status files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("update-worker must run as root")
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		req, found, err := webupdate.Take(*dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "update-worker: ignoring request:", err)
		}
		if found {
			done, err := runFleetUpdate(ctx, *dir, req)
			if err != nil {
				fmt.Fprintln(os.Stderr, "update-worker:", err)
			}
			if done || errors.Is(err, webupdate.ErrInstallationRestored) {
				// Exit so the service manager restarts the active on-disk binary,
				// whether activation committed or rollback restored the old one.
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func runReleaseInstaller(ctx context.Context, target, token string, args ...string) error {
	policy, err := releaseTrustFromEnvironment()
	if err != nil {
		return err
	}
	return runReleaseInstallerWithClient(ctx, target, token, policy, &http.Client{Timeout: 15 * time.Second}, version.Value, "/var/lib/payesh/payesh.db", args...)
}

func runReleaseInstallerWithClient(ctx context.Context, target, token string, policy releaseTrustPolicy, client *http.Client, currentCore, databasePath string, args ...string) error {
	if policy.mode == "preview" {
		fmt.Fprintln(os.Stderr, "warning: UNSIGNED PREVIEW update; publisher authenticity is not independently verified")
	}
	if policy.mode == "production" {
		prepared, err := prepareProductionRelease(ctx, client, token, target, currentCore, policy, databasePath)
		if err != nil {
			return err
		}
		return runPreparedReleaseInstaller(ctx, target, policy, prepared, args...)
	}
	script, err := fetchInstaller(ctx, client, token, target)
	if err != nil {
		return err
	}
	return executeReleaseInstaller(ctx, script, policy, args...)
}

func runPreparedReleaseInstaller(ctx context.Context, target string, policy releaseTrustPolicy, prepared preparedProductionRelease, args ...string) error {
	if policy.mode != "production" || prepared.Manifest.Release != target || len(prepared.ChecksumsSHA256) != 64 || len(prepared.Installer) == 0 {
		return errors.New("invalid prepared production release")
	}
	args = append(args, "--release-checksums-sha256", prepared.ChecksumsSHA256)
	return executeReleaseInstaller(ctx, prepared.Installer, policy, args...)
}

func executeReleaseInstaller(ctx context.Context, script []byte, policy releaseTrustPolicy, args ...string) error {
	dir, err := os.MkdirTemp("", "payesh-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, script, 0o600); err != nil {
		return err
	}
	args = append(args, "--release-mode", policy.mode)
	if policy.mode == "production" {
		args = append(args, "--release-public-key", policy.publicKey, "--release-key-id", policy.keyID)
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{path}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installer update failed: %w", err)
	}
	return nil
}

func installedRole(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read installed role: %w", err)
	}
	var state struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", fmt.Errorf("invalid install state: %w", err)
	}
	switch state.Role {
	case "standalone", "hub", "node", "cli-only":
		return state.Role, nil
	}
	return "", errors.New("installed role is invalid")
}

func fetchInstaller(ctx context.Context, client *http.Client, token, releaseVersion string) ([]byte, error) {
	if !updater.ValidRelease(releaseVersion) {
		return nil, errors.New("invalid release version")
	}
	policy, err := releaseTrustFromEnvironment()
	if err != nil {
		return nil, err
	}
	if policy.mode == "production" {
		return fetchAuthenticatedInstaller(ctx, client, token, releaseVersion, policy)
	}
	url := "https://raw.githubusercontent.com/" + release.DefaultRepository + "/v" + releaseVersion + "/install.sh"
	if token != "" {
		url = "https://api.github.com/repos/" + release.DefaultRepository + "/contents/install.sh?ref=v" + releaseVersion
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github.raw")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download installer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download installer returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 1<<20 || !strings.HasPrefix(string(data), "#!/bin/sh") {
		return nil, errors.New("GitHub returned an invalid installer")
	}
	return data, nil
}
