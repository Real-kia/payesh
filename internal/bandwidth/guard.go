package bandwidth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxCheckpointBytes = 1 << 20

// FileGuard persists watchdog checkpoints for a separate local recovery
// process. The directory must live on the node (normally /var/lib/payesh) and
// remain available without the hub or dashboard.
type FileGuard struct{ Dir string }

func (g FileGuard) Arm(_ context.Context, checkpoint Checkpoint) error {
	if !safeName.MatchString(checkpoint.ID) || checkpoint.ExpiresAt.IsZero() || len(checkpoint.Previous) > maxCheckpointBytes || (checkpoint.Action != Throttle && checkpoint.Action != Block) {
		return errors.New("invalid rollback checkpoint")
	}
	if err := checkpoint.Scope.Validate(); err != nil {
		return err
	}
	if g.Dir == "" {
		return errors.New("rollback directory is not configured")
	}
	data, err := json.Marshal(checkpoint)
	if err != nil || len(data) > maxCheckpointBytes {
		return errors.New("rollback checkpoint exceeds limit")
	}
	if err := os.MkdirAll(g.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(g.Dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(g.Dir, ".checkpoint-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	cleanup := func() { _ = os.Remove(temporaryName) }
	defer cleanup()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, g.path(checkpoint.ID)); err != nil {
		return err
	}
	return syncDirectory(g.Dir)
}

func (g FileGuard) Confirm(_ context.Context, id string) error {
	if !safeName.MatchString(id) || g.Dir == "" {
		return errors.New("invalid rollback checkpoint identity")
	}
	err := os.Remove(g.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("rollback checkpoint is missing")
	}
	if err != nil {
		return err
	}
	return syncDirectory(g.Dir)
}

// RecoverDue is called by the independent local watchdog. It reverts only
// expired checkpoints and removes each record only after owned cleanup was
// verified by Kernel.RevertOwned.
func (g FileGuard) RecoverDue(ctx context.Context, kernel Kernel, now time.Time) ([]string, error) {
	if kernel == nil || g.Dir == "" || now.IsZero() {
		return nil, errors.New("rollback recovery is not configured")
	}
	entries, err := os.ReadDir(g.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 128 {
		return nil, errors.New("rollback checkpoint directory exceeds limit")
	}
	recovered := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".checkpoint-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !safeName.MatchString(id) {
			return recovered, fmt.Errorf("unsafe rollback checkpoint filename %q", entry.Name())
		}
		path := g.path(id)
		info, err := os.Lstat(path)
		if err != nil {
			return recovered, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxCheckpointBytes {
			return recovered, fmt.Errorf("unsafe rollback checkpoint %q", id)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return recovered, err
		}
		var checkpoint Checkpoint
		if err := json.Unmarshal(data, &checkpoint); err != nil || checkpoint.ID != id || checkpoint.ExpiresAt.IsZero() || checkpoint.Scope.Validate() != nil || (checkpoint.Action != Throttle && checkpoint.Action != Block) {
			return recovered, fmt.Errorf("invalid rollback checkpoint %q", id)
		}
		if now.Before(checkpoint.ExpiresAt) {
			continue
		}
		if err := kernel.RevertOwned(ctx, checkpoint); err != nil {
			return recovered, fmt.Errorf("revert checkpoint %q: %w", id, err)
		}
		if err := os.Remove(path); err != nil {
			return recovered, err
		}
		recovered = append(recovered, id)
	}
	sort.Strings(recovered)
	if len(recovered) > 0 {
		if err := syncDirectory(g.Dir); err != nil {
			return recovered, err
		}
	}
	return recovered, nil
}

func (g FileGuard) path(id string) string { return filepath.Join(g.Dir, id+".json") }

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
