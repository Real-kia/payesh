package modules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/trust"
)

// DefaultHealthCheckTimeout bounds the post-install/post-enable capability
// check so a hung module process cannot block a lifecycle request forever.
const DefaultHealthCheckTimeout = 10 * time.Second

// HealthCheckFunc runs a module's own bounded self-check after staging. The
// real payesh-privd module.invoke("health") call is not implemented yet (see
// docs/handoffs/06.md); Manager accepts this as an injected function so a
// production server can supply the real check once that seam exists, while
// tests and interim deployments can supply a deterministic stub.
type HealthCheckFunc func(ctx context.Context, moduleID, installDir string) error

// Manager implements the package-06 lifecycle: eligibility, trust-verified
// staging, atomic activation, and durable state transitions. It never
// executes an archive's contents itself and never accepts a request-provided
// download URL; RootDir only ever receives bytes this process already
// verified against a catalog-approved manifest.
type Manager struct {
	Store       *monitoring.Store
	Trust       *trust.Registry
	RootDir     string
	HealthCheck HealthCheckFunc
	Now         func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}

func (m *Manager) installDir(serverID contracts.ServerID, moduleID string) string {
	return filepath.Join(m.RootDir, string(serverID), moduleID, "active")
}

func (m *Manager) stagingDir(serverID contracts.ServerID, moduleID, version string) string {
	return filepath.Join(m.RootDir, string(serverID), moduleID, "staged-"+version)
}

// InstallRequest carries everything an Install call needs to verify and
// stage one module release for one server. ManifestSignatureB64 signs the
// canonical JCS form of Manifest, per docs/contracts/RELEASE_FORMAT.md.
type InstallRequest struct {
	ServerID             contracts.ServerID
	ModuleID             string
	Manifest             contracts.ModuleManifest
	ManifestSignatureB64 string
	Archive              []byte
	ExpectedRevision     uint64
}

// Install runs the full eligibility → verify → stage → activate → health
// check sequence in one call, persisting a durable state transition at each
// stage so an interruption is always observable as a specific failed step
// rather than a silently stuck "installing" row. It intentionally lands on
// installed-disabled, never enabled: activation is a separate Enable call.
func (m *Manager) Install(ctx context.Context, req InstallRequest) (contracts.ModuleInstallation, error) {
	if req.ModuleID == "" || req.Manifest.ModuleID != req.ModuleID {
		return contracts.ModuleInstallation{}, errors.New("modules: module_id must match the manifest")
	}
	entry, ok := Lookup(req.ModuleID)
	if !ok {
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: %q is not an official catalog module", req.ModuleID)
	}
	if err := req.Manifest.Validate(); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	server, found, err := m.Store.GetServer(ctx, req.ServerID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if !found {
		return contracts.ModuleInstallation{}, errors.New("modules: server not found")
	}
	if eligibility := CheckEligibility(server, entry, req.Manifest); !eligibility.Eligible {
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: server is not eligible: %s", eligibility.Reason)
	}
	current, err := m.Store.GetModuleInstallation(ctx, req.ServerID, req.ModuleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != req.ExpectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}

	installation, err := m.advance(ctx, req.ServerID, req.ModuleID, current, EventDownloadStart, "", req.Manifest.ModuleVersion, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	installation, err = m.advance(ctx, req.ServerID, req.ModuleID, installation, EventVerifyStart, "", req.Manifest.ModuleVersion, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}

	verifyErr := m.verifyManifest(req)
	if verifyErr != nil {
		if _, failErr := m.advance(ctx, req.ServerID, req.ModuleID, installation, EventVerifyFailed, "", req.Manifest.ModuleVersion, wireError("manifest_verification_failed", verifyErr)); failErr != nil {
			return contracts.ModuleInstallation{}, failErr
		}
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: manifest verification failed: %w", verifyErr)
	}

	installation, err = m.advance(ctx, req.ServerID, req.ModuleID, installation, EventInstallStart, "", req.Manifest.ModuleVersion, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}

	staged := m.stagingDir(req.ServerID, req.ModuleID, req.Manifest.ModuleVersion)
	active := m.installDir(req.ServerID, req.ModuleID)
	if stageErr := m.stageAndActivate(ctx, req, staged, active); stageErr != nil {
		if _, failErr := m.advance(ctx, req.ServerID, req.ModuleID, installation, EventInstallFailed, "", req.Manifest.ModuleVersion, wireError("install_failed", stageErr)); failErr != nil {
			return contracts.ModuleInstallation{}, failErr
		}
		return contracts.ModuleInstallation{}, stageErr
	}

	return m.advance(ctx, req.ServerID, req.ModuleID, installation, EventInstallOK, "", req.Manifest.ModuleVersion, nil)
}

func (m *Manager) verifyManifest(req InstallRequest) error {
	canonical, err := trust.Canonicalize(req.Manifest)
	if err != nil {
		return err
	}
	return m.Trust.Verify(req.Manifest.SigningKeyID, canonical, req.ManifestSignatureB64, m.now())
}

func (m *Manager) stageAndActivate(ctx context.Context, req InstallRequest, staged, active string) error {
	if err := StageArchive(req.Archive, req.Manifest.CompressedBytes, req.Manifest.UnpackedBytes, req.Manifest.SHA256, staged); err != nil {
		return err
	}
	previousBackup, err := ActivateAtomic(staged, active)
	if err != nil {
		os.RemoveAll(staged)
		return err
	}
	check := m.HealthCheck
	if check == nil {
		check = func(context.Context, string, string) error { return nil }
	}
	healthCtx, cancel := context.WithTimeout(ctx, DefaultHealthCheckTimeout)
	defer cancel()
	if err := check(healthCtx, req.ModuleID, active); err != nil {
		// A failed health check is not success. Roll back to whatever was
		// active before, mirroring the release-update recovery contract.
		os.RemoveAll(active)
		if previousBackup != "" {
			os.Rename(previousBackup, active)
		}
		return fmt.Errorf("modules: health check failed: %w", err)
	}
	if previousBackup != "" {
		os.RemoveAll(previousBackup)
	}
	return nil
}

// Enable activates an installed-disabled module. It does not itself start
// any kernel/background work yet — that hook is the payesh-privd
// module.invoke wiring left for the next integration step (docs/handoffs/06.md).
func (m *Manager) Enable(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64) (contracts.ModuleInstallation, error) {
	return m.simpleTransition(ctx, serverID, moduleID, expectedRevision, EventEnable)
}

// Disable deactivates an enabled module back to installed-disabled.
func (m *Manager) Disable(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64) (contracts.ModuleInstallation, error) {
	return m.simpleTransition(ctx, serverID, moduleID, expectedRevision, EventDisable)
}

func (m *Manager) simpleTransition(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64, event Event) (contracts.ModuleInstallation, error) {
	current, err := m.Store.GetModuleInstallation(ctx, serverID, moduleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}
	return m.advance(ctx, serverID, moduleID, current, event, "", current.Version, nil)
}

// Remove tears down staged/active files and returns the module to
// unavailable. It is only legal once a module is installed-disabled or
// already failed: disable first stops userspace work and reverts active
// policies, which is enforced by the lifecycle table, not by this method.
func (m *Manager) Remove(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64) (contracts.ModuleInstallation, error) {
	current, err := m.Store.GetModuleInstallation(ctx, serverID, moduleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}
	removing, err := m.advance(ctx, serverID, moduleID, current, EventRemoveStart, "", current.Version, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	active := m.installDir(serverID, moduleID)
	removeErr := os.RemoveAll(active)
	if removeErr == nil {
		removeErr = os.RemoveAll(active + ".previous")
	}
	if removeErr != nil {
		// Recovery executables are intentionally left in place; a failed
		// cleanup must not delete the only copy an operator could recover.
		return m.advance(ctx, serverID, moduleID, removing, EventRemoveFailed, "", current.Version, wireError("remove_failed", removeErr))
	}
	return m.advance(ctx, serverID, moduleID, removing, EventRemoveOK, "", "", nil)
}

// advance validates the transition, then persists it with the store's own
// CAS so a concurrent lifecycle request for the same module cannot silently
// interleave with this one.
func (m *Manager) advance(ctx context.Context, serverID contracts.ServerID, moduleID string, current contracts.ModuleInstallation, event Event, before contracts.ModuleState, version string, moduleErr *contracts.Error) (contracts.ModuleInstallation, error) {
	next, err := NextState(current.State, event, before)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	return m.Store.TransitionModuleInstallation(ctx, serverID, moduleID, current.Revision, contracts.ModuleInstallation{
		Version: version, State: next, Error: moduleErr,
	})
}

func wireError(code string, err error) *contracts.Error {
	if err == nil {
		return nil
	}
	return &contracts.Error{Code: code, Message: err.Error()}
}
