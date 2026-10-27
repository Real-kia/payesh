package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// purgeChunkRows bounds each purge transaction so the single writer is free
// for node ingest between chunks instead of stalling for the whole delete.
const purgeChunkRows = 5000

// purgeTables are the high-volume rowid tables owned by a server. Everything
// else is small and left to the foreign-key cascade of the final delete.
var purgeTables = []string{"metric_rollups", "metric_samples", "log_entries", "port_traffic_observations"}

func (s *Store) isDeleting(id string) bool {
	_, ok := s.deleting.Load(id)
	return ok
}

func (s *Store) deletingIDs() []string {
	var ids []string
	s.deleting.Range(func(key, _ any) bool {
		ids = append(ids, key.(string))
		return true
	})
	return ids
}

// DeleteServerAsync validates the delete exactly like DeleteServer, hides the
// server immediately and removes its history in the background. A hub restart
// before the purge finishes leaves the (already revoked) server listed, and it
// can simply be deleted again.
func (s *Store) DeleteServerAsync(ctx context.Context, serverID contracts.ServerID, expectedRevision uint64) error {
	if !validStoreServerID(serverID) {
		return errors.New("server_id must be a bounded URL-safe identifier")
	}
	var role, revision string
	err := s.db.QueryRowContext(ctx, `SELECT role,configuration_revision FROM servers WHERE id=?`, string(serverID)).Scan(&role, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if role != "node" {
		return errors.New("server_role_not_deletable")
	}
	if revision != strconv.FormatUint(expectedRevision, 10) {
		return errors.New("configuration_revision_conflict")
	}
	if _, loaded := s.deleting.LoadOrStore(string(serverID), struct{}{}); loaded {
		return nil
	}
	go s.purgeServer(serverID)
	return nil
}

func (s *Store) purgeServer(serverID contracts.ServerID) {
	started := time.Now()
	id := string(serverID)
	ctx := context.Background()
	var purged int64
	failure := func() error {
		for _, table := range purgeTables {
			for {
				result, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid IN (SELECT rowid FROM `+table+` WHERE server_id=? LIMIT ?)`, id, purgeChunkRows)
				if err != nil {
					return err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return err
				}
				purged += n
				if n < purgeChunkRows {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		_, err := s.db.ExecContext(ctx, `DELETE FROM servers WHERE id=? AND role='node'`, id)
		return err
	}()
	if failure != nil {
		// Unhide so the server is visible and can be retried.
		s.deleting.Delete(id)
		log.Printf("server-purge: server=%s rows=%d duration_ms=%d error=%q", id, purged, time.Since(started).Milliseconds(), failure.Error())
		return
	}
	s.deleting.Delete(id)
	log.Printf("server-purge: server=%s rows=%d duration_ms=%d", id, purged, time.Since(started).Milliseconds())
}
