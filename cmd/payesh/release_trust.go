package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/trust"
	"github.com/Real-kia/payesh/internal/updater"
)

type releaseTrustPolicy struct{ mode, publicKey, keyID string }

func releaseTrustFromEnvironment() (releaseTrustPolicy, error) {
	p := releaseTrustPolicy{os.Getenv("PAYESH_RELEASE_MODE"), os.Getenv("PAYESH_RELEASE_PUBLIC_KEY"), os.Getenv("PAYESH_RELEASE_KEY_ID")}
	if p.mode == "" {
		p.mode = "production"
	}
	switch p.mode {
	case "production":
		if p.publicKey == "" || p.keyID == "" {
			return p, errors.New("production release requires externally configured PAYESH_RELEASE_PUBLIC_KEY and PAYESH_RELEASE_KEY_ID; legacy unsigned releases require explicit PAYESH_RELEASE_MODE=preview")
		}
		if err := release.ValidateKeyID(p.keyID); err != nil {
			return p, err
		}
		if p.keyID == "unavailable-local" {
			return p, errors.New("unsigned key ID is not a production trust anchor")
		}
		info, err := os.Lstat(p.publicKey)
		if err != nil {
			return p, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return p, errors.New("release anchor must be a regular file, not group/world writable")
		}
		if err := checkAnchorProtection(p.publicKey, info); err != nil {
			return p, err
		}
		if _, err := release.ReadProductionPublicKey(p.publicKey); err != nil {
			return p, err
		}
	case "preview":
		if p.publicKey != "" || p.keyID != "" {
			return p, errors.New("preview mode does not accept production trust inputs")
		}
	default:
		return p, errors.New("PAYESH_RELEASE_MODE must be production or preview")
	}
	return p, nil
}

// checkAnchorProtection requires the trust anchor and its directory to be
// controlled by root or the current account only. A file that another
// unprivileged account owns, or that sits in a directory it can write, could be
// replaced with a different key before a root worker verifies a release.
func checkAnchorProtection(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !anchorOwnerAllowed(stat.Uid, os.Geteuid()) {
		return errors.New("release anchor must be owned by root or the current account")
	}
	dir := filepath.Dir(path)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return err
	}
	dirStat, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || !anchorOwnerAllowed(dirStat.Uid, os.Geteuid()) || dirInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("release anchor directory must be owned by root or the current account and not group/world writable")
	}
	return nil
}

func anchorOwnerAllowed(owner uint32, euid int) bool {
	return owner == 0 || int(owner) == euid
}

// fetchAuthenticatedInstaller authenticates checksum metadata before a remote
// installer can execute with root privileges. The selected version and key ID
// are part of the signed payload, preventing cross-release or key-ID replay.
func fetchAuthenticatedInstaller(ctx context.Context, client *http.Client, token, target string, policy releaseTrustPolicy) ([]byte, error) {
	key, err := release.ReadProductionPublicKey(policy.publicKey)
	if err != nil {
		return nil, err
	}
	sums, err := fetchReleaseAsset(ctx, client, token, target, "SHA256SUMS")
	if err != nil {
		return nil, err
	}
	sig, err := fetchReleaseAsset(ctx, client, token, target, "SHA256SUMS.sig")
	if err != nil {
		return nil, err
	}
	if err := release.VerifyBootstrap(key, target, policy.keyID, sums, sig); err != nil {
		return nil, err
	}
	script, err := fetchReleaseAsset(ctx, client, token, target, "install.sh")
	if err != nil {
		return nil, err
	}
	if err := release.VerifyChecksumFile(sums, "install.sh", script); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(script), "#!/bin/sh") {
		return nil, errors.New("authenticated release installer has an unsupported interpreter")
	}
	return script, nil
}

func fetchReleaseAsset(ctx context.Context, client *http.Client, token, target, name string) ([]byte, error) {
	url := "https://github.com/" + release.DefaultRepository + "/releases/download/v" + target + "/" + name
	if token != "" {
		metaURL := "https://api.github.com/repos/" + release.DefaultRepository + "/releases/tags/v" + target
		metaReq, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
		if err != nil {
			return nil, err
		}
		metaReq.Header.Set("Authorization", "Bearer "+token)
		metaReq.Header.Set("Accept", "application/vnd.github+json")
		metaResp, err := client.Do(metaReq)
		if err != nil {
			return nil, err
		}
		defer metaResp.Body.Close()
		if metaResp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("release metadata returned HTTP %d", metaResp.StatusCode)
		}
		var metadata struct {
			Assets []struct{ Name, URL string } `json:"assets"`
		}
		if err := json.NewDecoder(io.LimitReader(metaResp.Body, 1<<20)).Decode(&metadata); err != nil {
			return nil, err
		}
		url = ""
		for _, asset := range metadata.Assets {
			if asset.Name == name {
				url = asset.URL
				break
			}
		}
		prefix := "https://api.github.com/repos/" + release.DefaultRepository + "/releases/assets/"
		if !strings.HasPrefix(url, prefix) || strings.Trim(strings.TrimPrefix(url, prefix), "0123456789") != "" || url == prefix {
			return nil, errors.New("release metadata lacks a safe GitHub asset URL")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/octet-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download release asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download release asset %s returned HTTP %d", name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > 1<<20 {
		return nil, errors.New("release bootstrap asset exceeds size limit or is empty")
	}
	return body, nil
}

type preparedProductionRelease struct {
	Manifest        contracts.ReleaseManifest
	ChecksumsSHA256 string
	Installer       []byte
}

// The authenticated checksum index binds manifest bytes, executable bootstrap,
// and every archive to one immutable candidate generation.
func prepareProductionRelease(ctx context.Context, client *http.Client, token, target, currentCore string, policy releaseTrustPolicy, databasePath string) (preparedProductionRelease, error) {
	var result preparedProductionRelease
	if policy.mode != "production" {
		return result, errors.New("candidate preparation requires production trust")
	}
	key, err := release.ReadProductionPublicKey(policy.publicKey)
	if err != nil {
		return result, err
	}
	sums, err := fetchReleaseAsset(ctx, client, token, target, "SHA256SUMS")
	if err != nil {
		return result, err
	}
	sig, err := fetchReleaseAsset(ctx, client, token, target, "SHA256SUMS.sig")
	if err != nil {
		return result, err
	}
	if err := release.VerifyBootstrap(key, target, policy.keyID, sums, sig); err != nil {
		return result, err
	}
	body, err := fetchReleaseAsset(ctx, client, token, target, "manifest.json")
	if err != nil {
		return result, err
	}
	if err := release.VerifyChecksumFile(sums, "manifest.json", body); err != nil {
		return result, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result.Manifest); err != nil {
		return result, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return result, errors.New("candidate manifest has trailing JSON")
	}
	m := result.Manifest
	if m.Release != target || m.SigningKeyID != policy.keyID {
		return result, errors.New("candidate manifest does not match requested release and anchor")
	}
	detached, err := fetchReleaseAsset(ctx, client, token, target, "manifest.json.sig")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(string(detached)) != string(detached) {
		return result, errors.New("candidate manifest signature has whitespace")
	}
	registry, err := trust.NewRegistry(trust.Anchor{KeyID: policy.keyID, PublicKey: key})
	if err != nil {
		return result, err
	}
	if err := updater.VerifyManifest(registry, m, string(detached), currentCore, updater.AcceptedState{}, time.Now().UTC()); err != nil {
		return result, err
	}
	entries, err := release.ParseChecksums(sums)
	if err != nil {
		return result, err
	}
	if len(entries) != len(m.Artifacts)+2 {
		return result, errors.New("candidate checksum inventory differs from manifest")
	}
	seen := map[string]bool{}
	for _, artifact := range m.Artifacts {
		name := filepath.Base(artifact.URL)
		if seen[name] || name == "manifest.json" || name == "install.sh" || entries[name] != artifact.SHA256 {
			return result, errors.New("candidate archive inventory differs from signed manifest")
		}
		seen[name] = true
	}
	if err := updater.CheckCandidateDatabaseSchema(ctx, m, databasePath); err != nil {
		return result, err
	}
	script, err := fetchReleaseAsset(ctx, client, token, target, "install.sh")
	if err != nil {
		return result, err
	}
	if err := release.VerifyChecksumFile(sums, "install.sh", script); err != nil {
		return result, err
	}
	if !strings.HasPrefix(string(script), "#!/bin/sh") || !strings.Contains(string(script), "PAYESH_RELEASE_BOUND_METADATA_V1=1") {
		return result, errors.New("candidate installer lacks authenticated metadata binding support")
	}
	digest := sha256.Sum256(sums)
	result.ChecksumsSHA256 = hex.EncodeToString(digest[:])
	result.Installer = script
	return result, nil
}
