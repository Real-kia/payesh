package monitoring

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// PortTrafficObservation is the durable projection of one owned nft counter.
// It intentionally has no nft-specific syntax so the monitoring store remains
// usable by a future privileged runtime implementation.
type PortTrafficObservation struct {
	ServerID   contracts.ServerID
	ScopeID    string
	Bytes      uint64
	Packets    uint64
	Generation string
	ObservedAt time.Time
	Continuity string
	Reason     string
}

func (s *Store) UpsertPortTrafficObservations(ctx context.Context, values []PortTrafficObservation) error {
	if s == nil || len(values) == 0 || len(values) > 256 {
		return errors.New("invalid port traffic observations")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := FormatPersistedTime(time.Now())
	for _, value := range values {
		if !validStoreServerID(value.ServerID) || value.ScopeID == "" || len(value.ScopeID) > 64 || value.Generation == "" || value.ObservedAt.IsZero() || value.Continuity == "" {
			return errors.New("invalid port traffic observation")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO port_traffic_observations(server_id,scope_id,bytes,packets,generation,observed_at,continuity,reason,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,scope_id) DO UPDATE SET bytes=excluded.bytes,packets=excluded.packets,generation=excluded.generation,observed_at=excluded.observed_at,continuity=excluded.continuity,reason=excluded.reason,updated_at=excluded.updated_at`, string(value.ServerID), value.ScopeID, strconv.FormatUint(value.Bytes, 10), strconv.FormatUint(value.Packets, 10), value.Generation, FormatPersistedTime(value.ObservedAt), value.Continuity, value.Reason, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListPortTrafficObservations(ctx context.Context, serverID contracts.ServerID) ([]PortTrafficObservation, error) {
	if s == nil || !validStoreServerID(serverID) {
		return nil, errors.New("invalid port traffic observation server")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope_id,bytes,packets,generation,observed_at,continuity,reason FROM port_traffic_observations WHERE server_id=? ORDER BY scope_id`, string(serverID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PortTrafficObservation, 0)
	for rows.Next() {
		var item PortTrafficObservation
		var bytesText, packetsText, observedAt string
		if err := rows.Scan(&item.ScopeID, &bytesText, &packetsText, &item.Generation, &observedAt, &item.Continuity, &item.Reason); err != nil {
			return nil, err
		}
		var err error
		item.Bytes, err = strconv.ParseUint(bytesText, 10, 64)
		if err != nil {
			return nil, err
		}
		item.Packets, err = strconv.ParseUint(packetsText, 10, 64)
		if err != nil {
			return nil, err
		}
		item.ObservedAt, err = time.Parse(persistedTimeLayout, observedAt)
		if err != nil {
			return nil, err
		}
		item.ServerID = serverID
		result = append(result, item)
	}
	return result, rows.Err()
}
