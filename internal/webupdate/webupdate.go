// Package webupdate is the file-based handoff between the unprivileged web
// hub and the root update worker. The hub can only ask for a release version;
// durable intent stays queued until the worker's authenticated installation
// transaction completes health verification or recovery.
package webupdate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/updater"
)

const (
	DefaultDir  = "/var/lib/payesh"
	RequestFile = "update-request.json"
	StatusFile  = "update-status.json"
	ServiceName = "payesh-update"

	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"

	// A queued or running state older than this is treated as abandoned.
	StaleAfter = 30 * time.Minute
	// LocalAuthorizationTTL bounds browser intent across worker restarts.
	LocalAuthorizationTTL = 30 * time.Minute
	maxFile               = 4096
)

var submitMu sync.Mutex

var ErrBusy = errors.New("an update is already in progress")

type Request struct {
	Version     string    `json:"version"`
	JobID       string    `json:"job_id,omitempty"`
	Deadline    time.Time `json:"deadline,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by,omitempty"`
}

type Status struct {
	Rollback  string    `json:"rollback,omitempty"`
	State     string    `json:"state"`
	JobID     string    `json:"job_id,omitempty"`
	Target    string    `json:"target,omitempty"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Active reports whether the status describes work that is still expected to run.
func (s Status) Active(now time.Time) bool {
	return (s.State == StateQueued || s.State == StateRunning) && now.Sub(s.UpdatedAt) < StaleAfter
}

// Supported reports whether the root worker service is installed.
func Supported(root string) bool {
	for _, path := range []string{"/etc/systemd/system/" + ServiceName + ".service", "/etc/init.d/" + ServiceName} {
		if info, err := os.Lstat(filepath.Join(root, path)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// Submit queues a durable local activation using the same protected worker
// transaction as fleet updates. Authorization expires even after a restart.
func Submit(dir string, req Request, now time.Time) error {
	if len(req.Version) > 64 || !updater.ValidRelease(req.Version) {
		return errors.New("invalid release version")
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return err
	}
	req.JobID = "web-local-" + hex.EncodeToString(identity[:])
	req.Deadline = now.UTC().Add(LocalAuthorizationTTL)
	return SubmitFleet(dir, req, now)
}

// Take reads a durable pending intent without removing its recovery handle.
// Older browser requests are promoted before the worker can activate anything.
func Take(dir string) (Request, bool, error) {
	return takeAt(dir, time.Now().UTC())
}

func takeAt(dir string, now time.Time) (Request, bool, error) {
	submitMu.Lock()
	defer submitMu.Unlock()
	path := filepath.Join(dir, RequestFile)
	var req Request
	if err := readJSON(path, &req); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Request{}, false, nil
		}
		_ = os.Remove(path)
		return Request{}, false, err
	}
	if len(req.Version) > 64 || !updater.ValidRelease(req.Version) {
		_ = os.Remove(path)
		return Request{}, false, errors.New("invalid release version in request")
	}
	if req.JobID == "" {
		if req.RequestedAt.IsZero() || req.RequestedAt.After(now) {
			return Request{}, false, errors.New("legacy update request has no valid authorization time; inspect and replace the retained request")
		}
		// Stable identity binds retries and concurrent readers to one root
		// journal. Promotion never grants a fresh deadline to old authorization.
		original, err := json.Marshal(req)
		if err != nil {
			return Request{}, false, err
		}
		digest := sha256.Sum256(original)
		req.JobID = "web-legacy-" + hex.EncodeToString(digest[:16])
		deadline := req.RequestedAt.UTC().Add(LocalAuthorizationTTL)
		if req.Deadline.IsZero() || req.Deadline.After(deadline) {
			req.Deadline = deadline
		}
		if err := writeJSON(dir, RequestFile, req, 0600); err != nil {
			return Request{}, false, fmt.Errorf("persist legacy update recovery intent: %w", err)
		}
	}
	if !validFleetRequest(req) {
		_ = os.Remove(path)
		return Request{}, false, errors.New("invalid fleet update intent")
	}
	return req, true, nil
}

func ReadStatus(dir string) (Status, error) {
	var status Status
	err := readJSON(filepath.Join(dir, StatusFile), &status)
	return status, err
}

func WriteStatus(dir string, status Status) error {
	if len(status.Message) > 512 {
		status.Message = status.Message[:512]
	}
	return writeJSON(dir, StatusFile, status, 0o644)
}

// readJSON refuses symlinks and special files: the directory is writable by the
// unprivileged service account while the worker runs as root.
func readJSON(path string, v any) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFile {
		return fmt.Errorf("%s is not a small regular file", filepath.Base(path))
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return errors.New("update file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return err
	}
	if len(data) > maxFile {
		return errors.New("update file is too large")
	}
	return json.Unmarshal(data, v)
}

// writeJSON uses an exclusive temp file and rename, which replaces a planted
// symlink instead of following it.
func writeJSON(dir, name string, v any, mode os.FileMode) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".update-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
