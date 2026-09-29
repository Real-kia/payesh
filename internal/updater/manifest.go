// Package updater implements release verification and recovery primitives.
// Network fetching and service activation are intentionally separate: no
// downloaded byte is trusted until this package accepts its signed manifest
// and verifies the selected artifact.
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/trust"
)

var (
	ErrManifestReplay      = errors.New("updater: release manifest is stale or replayed")
	ErrArtifactUnavailable = errors.New("updater: matching release artifact is unavailable")
	semverPattern          = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?$`)
)

type AcceptedState struct {
	Release   string    `json:"release"`
	CreatedAt time.Time `json:"created_at"`
}

// VerifyManifest verifies the detached signature and all policy fields before
// comparing it with durable last-accepted metadata. Equality is permitted so
// an interrupted download can safely resume; older metadata is rejected.
func VerifyManifest(registry *trust.Registry, manifest contracts.ReleaseManifest, signature string, currentCore string, state AcceptedState, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := validateManifest(manifest, currentCore, now); err != nil {
		return err
	}
	canonical, err := trust.Canonicalize(manifest)
	if err != nil {
		return fmt.Errorf("updater: canonicalize manifest: %w", err)
	}
	if err := registry.Verify(manifest.SigningKeyID, canonical, signature, now); err != nil {
		return fmt.Errorf("updater: verify manifest: %w", err)
	}
	if !state.CreatedAt.IsZero() {
		if !validSemver(state.Release) {
			return errors.New("updater: accepted-state release is invalid")
		}
		cmp := compareSemver(manifest.Release, state.Release)
		if manifest.CreatedAt.Before(state.CreatedAt) || cmp < 0 || (manifest.CreatedAt.Equal(state.CreatedAt) && cmp != 0) {
			return ErrManifestReplay
		}
	}
	return nil
}

func validateManifest(m contracts.ReleaseManifest, currentCore string, now time.Time) error {
	if m.Format != contracts.ReleaseFormat || !validSemver(m.Release) || !validSemver(m.MinCore) || m.SigningKeyID == "" {
		return errors.New("updater: invalid release manifest identity")
	}
	if !validSemver(currentCore) || compareSemver(currentCore, m.MinCore) < 0 {
		return errors.New("updater: current core is below the manifest minimum")
	}
	if m.CreatedAt.IsZero() || m.CreatedAt.After(now.Add(10*time.Minute)) {
		return errors.New("updater: invalid manifest creation time")
	}
	if len(m.Artifacts) == 0 || len(m.Artifacts) > 128 {
		return errors.New("updater: invalid artifact inventory")
	}
	seen := make(map[string]struct{}, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		key := artifact.Name + "\x00" + artifact.OS + "\x00" + artifact.Arch
		_, duplicate := seen[key]
		seen[key] = struct{}{}
		if duplicate || artifact.Name == "" || artifact.OS == "" || artifact.Arch == "" || artifact.URL == "" || artifact.CompressedBytes == 0 || artifact.UnpackedBytes == 0 {
			return errors.New("updater: invalid release artifact")
		}
		if len(artifact.SHA256) != sha256.Size*2 {
			return errors.New("updater: invalid artifact digest")
		}
		if _, err := hex.DecodeString(artifact.SHA256); err != nil || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
			return errors.New("updater: invalid artifact digest")
		}
	}
	return nil
}

// SelectArtifact fails closed when a release does not explicitly contain the
// requested OS/architecture/name tuple.
func SelectArtifact(m contracts.ReleaseManifest, name, goos, goarch string) (contracts.ReleaseArtifact, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	for _, artifact := range m.Artifacts {
		if artifact.Name == name && artifact.OS == goos && artifact.Arch == goarch {
			return artifact, nil
		}
	}
	return contracts.ReleaseArtifact{}, ErrArtifactUnavailable
}

func VerifyArtifact(artifact contracts.ReleaseArtifact, body []byte) error {
	if uint64(len(body)) != artifact.CompressedBytes {
		return errors.New("updater: artifact size mismatch")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return errors.New("updater: artifact checksum mismatch")
	}
	return nil
}

func LoadAcceptedState(path string) (AcceptedState, error) {
	var state AcceptedState
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(body, &state); err != nil || !validSemver(state.Release) || state.CreatedAt.IsZero() {
		return AcceptedState{}, errors.New("updater: accepted-state file is invalid")
	}
	return state, nil
}

func SaveAcceptedState(path string, state AcceptedState) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !validSemver(state.Release) || state.CreatedAt.IsZero() {
		return errors.New("updater: invalid accepted-state destination")
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".accepted-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func validSemver(version string) bool { return semverPattern.MatchString(version) }

// CompareReleases compares validated release versions. It returns -1, 0, or 1.
func CompareReleases(a, b string) (int, error) {
	if !validSemver(a) || !validSemver(b) {
		return 0, errors.New("invalid release version")
	}
	return compareSemver(a, b), nil
}

func compareSemver(a, b string) int {
	am, bm := semverPattern.FindStringSubmatch(a), semverPattern.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		av, _ := strconv.ParseUint(am[i], 10, 64)
		bv, _ := strconv.ParseUint(bm[i], 10, 64)
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	if am[4] == "" && bm[4] != "" {
		return 1
	}
	if am[4] != "" && bm[4] == "" {
		return -1
	}
	return strings.Compare(am[4], bm[4])
}
