package monitoring

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// LocationCandidate is everything the country resolver needs to decide
// whether, and with which address, a server should be looked up.
type LocationCandidate struct {
	ServerID      contracts.ServerID
	Role          string
	Address       string
	PeerIP        string
	CountryCode   string
	LookupIP      string
	Attempts      int
	NextAttemptAt time.Time
	UpdatedAt     time.Time
}

// RecordServerPeerIP remembers the public address a node last connected
// from, so nodes enrolled without an address can still be located. It only
// writes when the address changed.
func (s *Store) RecordServerPeerIP(ctx context.Context, serverID contracts.ServerID, ip netip.Addr) error {
	if !validStoreServerID(serverID) || !ip.IsValid() {
		return errors.New("a valid server id and address are required")
	}
	now := FormatPersistedTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO server_locations(server_id,peer_ip,updated_at) SELECT id,?,? FROM servers WHERE id=?
ON CONFLICT(server_id) DO UPDATE SET peer_ip=excluded.peer_ip WHERE server_locations.peer_ip<>excluded.peer_ip`, ip.Unmap().String(), now, string(serverID))
	return err
}

// LocationCandidates lists every server with its lookup bookkeeping.
func (s *Store) LocationCandidates(ctx context.Context, limit int) ([]LocationCandidate, error) {
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT servers.id,servers.role,COALESCE(servers.address,''),COALESCE(l.peer_ip,''),COALESCE(l.country_code,''),COALESCE(l.lookup_ip,''),COALESCE(l.attempts,0),COALESCE(l.next_attempt_at,''),COALESCE(l.updated_at,'')
FROM servers LEFT JOIN server_locations l ON l.server_id=servers.id ORDER BY servers.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocationCandidate
	for rows.Next() {
		var c LocationCandidate
		var id, next, updated string
		if err := rows.Scan(&id, &c.Role, &c.Address, &c.PeerIP, &c.CountryCode, &c.LookupIP, &c.Attempts, &next, &updated); err != nil {
			return nil, err
		}
		c.ServerID = contracts.ServerID(id)
		// Unparseable bookkeeping times are treated as "due now".
		c.NextAttemptAt, _ = time.Parse(time.RFC3339Nano, next)
		c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveServerCountry stores a successful lookup and clears the failure count.
func (s *Store) SaveServerCountry(ctx context.Context, serverID contracts.ServerID, countryCode, lookupIP string, refreshAt time.Time) error {
	if !validStoreServerID(serverID) || !validCountryCode(countryCode) {
		return errors.New("a valid server id and country code are required")
	}
	now := FormatPersistedTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO server_locations(server_id,country_code,lookup_ip,attempts,next_attempt_at,updated_at) SELECT id,?,?,0,?,? FROM servers WHERE id=?
ON CONFLICT(server_id) DO UPDATE SET country_code=excluded.country_code,lookup_ip=excluded.lookup_ip,attempts=0,next_attempt_at=excluded.next_attempt_at,updated_at=excluded.updated_at`,
		countryCode, lookupIP, FormatPersistedTime(refreshAt.UTC()), now, string(serverID))
	return err
}

// RecordServerCountryFailure schedules the next attempt. A previously found
// country is kept unless clear is set (the server's address changed).
func (s *Store) RecordServerCountryFailure(ctx context.Context, serverID contracts.ServerID, lookupIP string, attempts int, nextAttempt time.Time, clear bool) error {
	if !validStoreServerID(serverID) {
		return errors.New("a valid server id is required")
	}
	now := FormatPersistedTime(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO server_locations(server_id,lookup_ip,attempts,next_attempt_at,updated_at) SELECT id,?,?,?,? FROM servers WHERE id=?
ON CONFLICT(server_id) DO UPDATE SET country_code=CASE WHEN ? THEN '' ELSE server_locations.country_code END,lookup_ip=excluded.lookup_ip,attempts=excluded.attempts,next_attempt_at=excluded.next_attempt_at,updated_at=excluded.updated_at`,
		lookupIP, attempts, FormatPersistedTime(nextAttempt.UTC()), now, string(serverID), clear)
	return err
}

func validCountryCode(code string) bool {
	return len(code) == 2 && code[0] >= 'A' && code[0] <= 'Z' && code[1] >= 'A' && code[1] <= 'Z'
}
