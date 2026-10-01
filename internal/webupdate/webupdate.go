// Package webupdate is the file-based handoff between the unprivileged web
// hub and the root update worker. The hub can only ask for a release version;
// the worker validates it and runs the normal release installer.
package webupdate

import (
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
	maxFile    = 4096
)

var submitMu sync.Mutex

var ErrBusy = errors.New("an update is already in progress")

type Request struct {
	Version     string    `json:"version"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by,omitempty"`
}

type Status struct {
	State     string    `json:"state"`
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

// Submit queues a request. It fails with ErrBusy while another is active.
func Submit(dir string, req Request, now time.Time) error {
	submitMu.Lock()
	defer submitMu.Unlock()
	if len(req.Version) > 64 || !updater.ValidRelease(req.Version) {
		return errors.New("invalid release version")
	}
	if status, err := ReadStatus(dir); err == nil && status.Active(now) {
		return ErrBusy
	}
	req.RequestedAt = now.UTC()
	// Publish queued status before the request so a fast worker cannot have its
	// running status overwritten by the producer.
	if err := WriteStatus(dir, Status{State: StateQueued, Target: req.Version, UpdatedAt: now.UTC()}); err != nil {
		return err
	}
	if err := writeJSON(dir, RequestFile, req, 0o600); err != nil {
		_ = WriteStatus(dir, Status{State: StateFailed, Target: req.Version, Message: "could not queue update request", UpdatedAt: now.UTC()})
		return err
	}
	return nil
}

// Take reads and removes a pending request. A malformed request is discarded.
func Take(dir string) (Request, bool, error) {
	path := filepath.Join(dir, RequestFile)
	var req Request
	if err := readJSON(path, &req); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Request{}, false, nil
		}
		_ = os.Remove(path)
		return Request{}, false, err
	}
	_ = os.Remove(path)
	if len(req.Version) > 64 || !updater.ValidRelease(req.Version) {
		return Request{}, false, errors.New("invalid release version in request")
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}
