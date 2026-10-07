package cutoverpeer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// MaxGrantLifetime bounds how long a grant can live: a cutover is a short,
// supervised operation, so a grant that outlives it is a mistake.
const MaxGrantLifetime = 24 * time.Hour

type grantRecord struct {
	ClientFingerprint string    `json:"client_fingerprint"`
	ServerID          string    `json:"server_id"`
	CutoverID         string    `json:"cutover_id"`
	ExpiresAt         time.Time `json:"expires_at"`
}

func (g Grant) validate() error {
	if !hexDigest.MatchString(g.ClientFingerprint) {
		return errors.New("cutoverpeer: the client fingerprint must be lowercase SHA-256 hex")
	}
	if !cutoverName.MatchString(g.CutoverID) {
		return errors.New("cutoverpeer: the cutover identifier is invalid")
	}
	if g.ServerID == "" || len(g.ServerID) > 128 {
		return errors.New("cutoverpeer: a server identifier is required")
	}
	return nil
}

// LoadGrants reads a grant file. A missing file means no grants; a corrupt or
// group/world-readable file is an error, so a damaged file never silently becomes
// either "allow" or "deny" without the operator noticing.
func LoadGrants(path string) ([]Grant, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("cutoverpeer: the grant file must be a regular file readable only by its owner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var records []grantRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("cutoverpeer: the grant file is corrupt: %w", err)
	}
	grants := make([]Grant, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		// One live grant per client keeps "which cutover is this request for"
		// unambiguous without ever reading it from the request.
		if seen[record.ClientFingerprint] {
			return nil, errors.New("cutoverpeer: the grant file has more than one grant for a client certificate")
		}
		seen[record.ClientFingerprint] = true
		grant := Grant{ClientFingerprint: record.ClientFingerprint, ServerID: contracts.ServerID(record.ServerID), CutoverID: record.CutoverID, ExpiresAt: record.ExpiresAt}
		if err := grant.validate(); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, nil
}

// AddGrant validates a grant and records it under an exclusive lock. It replaces
// any earlier grant for the same client certificate and drops expired grants. The file is replaced atomically.
func AddGrant(path string, grant Grant, now time.Time) error {
	if err := grant.validate(); err != nil {
		return err
	}
	if !grant.ExpiresAt.After(now) || grant.ExpiresAt.After(now.Add(MaxGrantLifetime)) {
		return fmt.Errorf("cutoverpeer: a grant must expire within %s and in the future", MaxGrantLifetime)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	existing, err := LoadGrants(path)
	if err != nil {
		return err
	}
	records := make([]grantRecord, 0, len(existing)+1)
	for _, current := range existing {
		// A new grant supersedes the client's previous one; expired grants are dropped.
		if now.Before(current.ExpiresAt) && current.ClientFingerprint != grant.ClientFingerprint {
			records = append(records, grantRecord{current.ClientFingerprint, string(current.ServerID), current.CutoverID, current.ExpiresAt})
		}
	}
	records = append(records, grantRecord{grant.ClientFingerprint, string(grant.ServerID), grant.CutoverID, grant.ExpiresAt})
	encoded, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
