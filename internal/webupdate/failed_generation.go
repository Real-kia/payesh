package webupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Raw database/WAL files are retained together with node replay identities.
// They are recovery evidence, never opened or automatically merged by rollback.
func failedGenerationNames() []string {
	return []string{"payesh.db", "payesh.db-wal", "payesh.db-shm", "agent.spool", "node-identity.json", "node-identity.json.transport.json", "server-id", "hub-ca.pem"}
}

type failedGenerationFile struct {
	Exists bool   `json:"exists"`
	SHA256 string `json:"sha256,omitempty"`
}

func failedGenerationDigest(path string, limit uint64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	uid, _ := fileOwner(info)
	if !info.Mode().IsRegular() || uint64(info.Size()) > limit || info.Mode().Perm() != 0600 || uid != os.Geteuid() {
		return "", errors.New("unsafe failed-generation file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("failed-generation file changed while opening")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if uint64(n) > limit {
		return "", errors.New("failed-generation file exceeds limit")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (t InstallationTransaction) verifyFailedGeneration(ctx context.Context, dir string) error {
	if err := safeExistingParents(dir, t.Root == "/"); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("unsafe failed-generation directory")
	}
	p := filepath.Join(dir, "manifest.json")
	info, err = os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 || info.Mode().Perm() != 0600 {
		return errors.New("missing or unsafe failed-generation manifest")
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var files map[string]failedGenerationFile
	if err = json.Unmarshal(body, &files); err != nil {
		return err
	}
	if len(files) != len(failedGenerationNames()) {
		return errors.New("incomplete failed-generation inventory")
	}
	limit, _ := t.limits()
	if _, err = snapshotTreeBytes(dir, limit); err != nil {
		return err
	}
	for _, name := range failedGenerationNames() {
		if err = ctx.Err(); err != nil {
			return err
		}
		file, ok := files[name]
		if !ok {
			return errors.New("invalid failed-generation inventory")
		}
		p := filepath.Join(dir, name)
		if !file.Exists {
			if _, err = os.Lstat(p); !errors.Is(err, os.ErrNotExist) || file.SHA256 != "" {
				return errors.New("invalid absent failed-generation file")
			}
			continue
		}
		digest, err := failedGenerationDigest(p, limit)
		if err != nil {
			return err
		}
		if digest != file.SHA256 {
			return fmt.Errorf("failed-generation digest mismatch: %s", name)
		}
	}
	return nil
}

func syncGenerationDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (t InstallationTransaction) preserveFailedGeneration(ctx context.Context, allowCapture bool) error {
	dir := filepath.Join(t.dir(), "failed-generation")
	if _, err := os.Lstat(dir); err == nil {
		return t.verifyFailedGeneration(ctx, dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !allowCapture {
		return errors.New("restoration already started without retained failed generation")
	}
	limit, budget := t.limits()
	var needed uint64
	for _, name := range failedGenerationNames() {
		p := t.path("/var/lib/payesh/" + name)
		if err := safeExistingParents(filepath.Dir(p), false); err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("candidate data must be regular non-symlink files")
		}
		if uint64(info.Size()) > limit || needed > limit-uint64(info.Size()) {
			return errors.New("failed generation exceeds snapshot limit")
		}
		needed += uint64(info.Size())
	}
	// Include archive metadata, retained snapshots and restoration staging.
	var largest uint64
	for _, p := range installationPaths() {
		n, err := snapshotTreeBytes(filepath.Join(t.dir(), "files", p[1:]), limit)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if n > largest {
			largest = n
		}
	}
	required := needed + largest + snapshotReserve + 16384
	retained, err := snapshotTreeBytes(filepath.Dir(t.dir()), budget)
	if err != nil {
		return err
	}
	if required > budget || retained > budget-required {
		return errors.New("failed-generation archive budget exhausted")
	}
	available, err := t.available(t.dir())
	if err != nil {
		return err
	}
	if available < required {
		return errors.New("insufficient space to preserve candidate and restore")
	}
	stage, err := os.MkdirTemp(t.dir(), ".failed-generation-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if limit < 16384 {
		return errors.New("failed-generation metadata exceeds snapshot limit")
	}
	remaining := limit - 16384
	files := map[string]failedGenerationFile{}
	for _, name := range failedGenerationNames() {
		if err = ctx.Err(); err != nil {
			return err
		}
		source := t.path("/var/lib/payesh/" + name)
		if _, err = os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			files[name] = failedGenerationFile{}
			continue
		} else if err != nil {
			return err
		}
		dest := filepath.Join(stage, name)
		if err = copyInstallationTreeBounded(source, dest, &remaining, t.beforeSnapshotOpen); err != nil {
			return err
		}
		// Evidence remains private even when the service source was permissive.
		if err = os.Chmod(dest, 0600); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err = os.Chown(dest, 0, 0); err != nil {
				return err
			}
		}
		digest, err := failedGenerationDigest(dest, limit)
		if err != nil {
			return err
		}
		files[name] = failedGenerationFile{Exists: true, SHA256: digest}
	}
	if err = writeJSON(stage, "manifest.json", files, 0600); err != nil {
		return err
	}
	if err = syncGenerationDirectory(stage); err != nil {
		return err
	}
	if err = os.Rename(stage, dir); err != nil {
		return err
	}
	if err = syncGenerationDirectory(t.dir()); err != nil {
		return err
	}
	return t.verifyFailedGeneration(ctx, dir)
}
