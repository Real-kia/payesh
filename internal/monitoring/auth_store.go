package monitoring

import (
	"database/sql"
	"errors"
	"fmt"
)

// Bound the snapshot including 256 tokens, each with 100 bounded activity
// entries and up to 200 server scopes. Older snapshots remain compatible.
const maxAuthStateBytes = 32 << 20
const maxIdentityStateBytes = 4 << 20

// LoadAuthState implements auth.Repository without exposing the database to
// request handlers. A single row is replaced atomically on every auth change.
func (s *Store) LoadAuthState() ([]byte, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("authentication store is unavailable")
	}
	var data []byte
	err := s.db.QueryRow(`SELECT state_json FROM browser_auth_state WHERE singleton=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load browser authentication state: %w", err)
	}
	if len(data) == 0 || len(data) > maxAuthStateBytes {
		return nil, false, errors.New("browser authentication state has invalid size")
	}
	return append([]byte(nil), data...), true, nil
}

func (s *Store) LoadIdentityState() ([]byte, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("identity store is unavailable")
	}
	var data []byte
	err := s.db.QueryRow(`SELECT state_json FROM fleet_identity_state WHERE singleton=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load fleet identity state: %w", err)
	}
	if len(data) == 0 || len(data) > maxIdentityStateBytes {
		return nil, false, errors.New("fleet identity state has invalid size")
	}
	return append([]byte(nil), data...), true, nil
}

func (s *Store) SaveIdentityState(data []byte) error {
	if s == nil || s.db == nil {
		return errors.New("identity store is unavailable")
	}
	if len(data) == 0 || len(data) > maxIdentityStateBytes {
		return errors.New("fleet identity state has invalid size")
	}
	_, err := s.db.Exec(`INSERT INTO fleet_identity_state(singleton,state_json) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET state_json=excluded.state_json`, data)
	if err != nil {
		return fmt.Errorf("save fleet identity state: %w", err)
	}
	return nil
}

func (s *Store) SaveAuthState(data []byte) error {
	if s == nil || s.db == nil {
		return errors.New("authentication store is unavailable")
	}
	if len(data) == 0 || len(data) > maxAuthStateBytes {
		return errors.New("browser authentication state has invalid size")
	}
	_, err := s.db.Exec(`INSERT INTO browser_auth_state(singleton,state_json) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET state_json=excluded.state_json`, data)
	if err != nil {
		return fmt.Errorf("save browser authentication state: %w", err)
	}
	return nil
}
