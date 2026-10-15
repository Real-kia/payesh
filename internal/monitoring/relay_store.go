package monitoring

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// DefaultRelayPort is the TCP port a relay node listens on unless the hub
// already assigned another one.
const DefaultRelayPort = 9798

// RelayGrantRecord is one stored relay credential.
type RelayGrantRecord struct {
	RelayServerID  contracts.ServerID
	Username       string
	PasswordSHA256 string
	Destinations   []string
	ExpiresAt      time.Time // zero means no expiry
	TargetServerID contracts.ServerID
}

// EnsureRelayNode marks serverID as a relay and returns its port. An existing
// relay keeps its port.
func (s *Store) EnsureRelayNode(ctx context.Context, serverID contracts.ServerID, port int, now time.Time) (int, error) {
	if !validStoreServerID(serverID) {
		return 0, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if port < 1 || port > 65535 {
		return 0, errors.New("relay port is invalid")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO relay_nodes(server_id,port,updated_at) VALUES(?,?,?) ON CONFLICT(server_id) DO NOTHING`, string(serverID), port, FormatPersistedTime(now)); err != nil {
		return 0, err
	}
	var current int
	err := s.db.QueryRowContext(ctx, `SELECT port FROM relay_nodes WHERE server_id=?`, string(serverID)).Scan(&current)
	return current, err
}

// PutRelayGrant creates or replaces one credential on a relay.
func (s *Store) PutRelayGrant(ctx context.Context, grant RelayGrantRecord, now time.Time) error {
	if !validStoreServerID(grant.RelayServerID) || grant.Username == "" || len(grant.Username) > 128 || len(grant.PasswordSHA256) != 64 || len(grant.Destinations) == 0 || len(grant.Destinations) > 16 {
		return errors.New("relay grant is invalid")
	}
	destinations, err := json.Marshal(grant.Destinations)
	if err != nil {
		return err
	}
	expires := ""
	if !grant.ExpiresAt.IsZero() {
		expires = FormatPersistedTime(grant.ExpiresAt)
	}
	var target any
	if grant.TargetServerID != "" {
		target = string(grant.TargetServerID)
	}
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM relay_grants WHERE relay_server_id=? AND username<>?`, string(grant.RelayServerID), grant.Username).Scan(&count); err != nil {
			return err
		}
		if count >= contracts.MaxRelayGrants {
			return errors.New("relay has too many credentials")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO relay_grants(relay_server_id,username,password_sha256,destinations_json,expires_at,target_server_id,created_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(relay_server_id,username) DO UPDATE SET password_sha256=excluded.password_sha256,destinations_json=excluded.destinations_json,expires_at=excluded.expires_at,target_server_id=excluded.target_server_id,created_at=excluded.created_at`,
			string(grant.RelayServerID), grant.Username, grant.PasswordSHA256, string(destinations), expires, target, FormatPersistedTime(now))
		return err
	})
}

// DeleteRelayGrant removes one credential; a missing one is not an error.
func (s *Store) DeleteRelayGrant(ctx context.Context, relay contracts.ServerID, username string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM relay_grants WHERE relay_server_id=? AND username=?`, string(relay), username)
	return err
}

// RelayPolicy returns what the agent on serverID should run. A server that is
// not a relay gets Enabled=false so a previously configured relay stops.
func (s *Store) RelayPolicy(ctx context.Context, serverID contracts.ServerID, now time.Time) (*contracts.RelayPolicy, error) {
	var port int
	err := s.readDB().QueryRowContext(ctx, `SELECT port FROM relay_nodes WHERE server_id=?`, string(serverID)).Scan(&port)
	if errors.Is(err, sql.ErrNoRows) {
		return &contracts.RelayPolicy{Enabled: false}, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.readDB().QueryContext(ctx, `SELECT username,password_sha256,destinations_json,expires_at FROM relay_grants WHERE relay_server_id=? AND (expires_at='' OR expires_at>?) ORDER BY username LIMIT ?`, string(serverID), FormatPersistedTime(now), contracts.MaxRelayGrants)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policy := &contracts.RelayPolicy{Enabled: true, Port: port, Grants: []contracts.RelayGrant{}}
	for rows.Next() {
		var grant contracts.RelayGrant
		var destinations, expires string
		if err := rows.Scan(&grant.Username, &grant.PasswordSHA256, &destinations, &expires); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(destinations), &grant.Destinations); err != nil {
			return nil, err
		}
		if expires != "" {
			parsed, err := time.Parse(time.RFC3339Nano, expires)
			if err != nil {
				return nil, err
			}
			grant.ExpiresAt = &parsed
		}
		policy.Grants = append(policy.Grants, grant)
	}
	return policy, rows.Err()
}

// PruneExpiredRelayGrants deletes credentials past their expiry.
func (s *Store) PruneExpiredRelayGrants(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM relay_grants WHERE expires_at<>'' AND expires_at<=?`, FormatPersistedTime(now))
	return err
}

// RelayNodeIDs lists servers configured as relays.
func (s *Store) RelayNodeIDs(ctx context.Context) ([]contracts.ServerID, error) {
	rows, err := s.readDB().QueryContext(ctx, `SELECT server_id FROM relay_nodes ORDER BY server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []contracts.ServerID{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, contracts.ServerID(strings.TrimSpace(id)))
	}
	return ids, rows.Err()
}

// RemoveRelayNode stops serverID from relaying; its credentials go with it.
// Servers that reach the hub through it lose that path.
func (s *Store) RemoveRelayNode(ctx context.Context, serverID contracts.ServerID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM relay_nodes WHERE server_id=?`, string(serverID))
	return err
}

// RelayInfo describes a configured relay for the dashboard.
type RelayInfo struct {
	ServerID        contracts.ServerID `json:"server_id"`
	Name            string             `json:"name"`
	Address         string             `json:"address"`
	Port            int                `json:"port"`
	ConnectionState string             `json:"connection_state"`
	// Nodes counts servers that keep a permanent credential on this relay.
	Nodes int `json:"nodes"`
}

// ListRelays returns configured relays, most used first.
func (s *Store) ListRelays(ctx context.Context) ([]RelayInfo, error) {
	rows, err := s.readDB().QueryContext(ctx, `SELECT r.server_id,s.name,s.address,r.port,s.connection_state,
(SELECT count(*) FROM relay_grants g WHERE g.relay_server_id=r.server_id AND g.expires_at='')
FROM relay_nodes r JOIN servers s ON s.id=r.server_id ORDER BY 6 DESC, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relays := []RelayInfo{}
	for rows.Next() {
		var info RelayInfo
		var id string
		if err := rows.Scan(&id, &info.Name, &info.Address, &info.Port, &info.ConnectionState, &info.Nodes); err != nil {
			return nil, err
		}
		info.ServerID = contracts.ServerID(id)
		relays = append(relays, info)
	}
	return relays, rows.Err()
}

// TouchRelayUse records that serverID connected from ip; when ip belongs to
// one of its relays, that relay credential is in use.
func (s *Store) TouchRelayUse(ctx context.Context, serverID contracts.ServerID, ip string, now time.Time) error {
	if ip == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE relay_grants SET last_used_at=? WHERE target_server_id=? AND expires_at='' AND relay_server_id IN (
 SELECT s.id FROM servers s LEFT JOIN server_locations l ON l.server_id=s.id WHERE s.address=? OR l.peer_ip=?)`, FormatPersistedTime(now), string(serverID), ip, ip)
	return err
}

// PruneUnusedRelayGrants removes permanent node credentials not used through
// the relay for the given number of days (never-used ones count from their
// creation). days <= 0 keeps them.
func (s *Store) PruneUnusedRelayGrants(ctx context.Context, now time.Time, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := FormatPersistedTime(now.Add(-time.Duration(days) * 24 * time.Hour))
	result, err := s.db.ExecContext(ctx, `DELETE FROM relay_grants WHERE expires_at='' AND (CASE WHEN last_used_at<>'' THEN last_used_at ELSE created_at END) < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
