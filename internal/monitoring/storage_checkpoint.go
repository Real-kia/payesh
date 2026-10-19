package monitoring

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// checkpointForStorage reclaims journal space before pressure deletes history.
// A reader can pin a WAL snapshot indefinitely. Waiting for it with SQLite's
// normal write timeout blocks the single writer, and deleting more rows only
// grows that pinned journal. Report contention immediately instead.
func (s *Store) checkpointForStorage(ctx context.Context) (reclaimed bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	var timeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return false, err
	}
	defer func() {
		// The caller may cancel during checkpointing. Always restore the normal
		// write policy before this pooled connection is used by another caller.
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_, restoreErr := conn.ExecContext(restoreCtx, fmt.Sprintf("PRAGMA busy_timeout=%d", timeout))
		err = errors.Join(err, restoreErr)
	}()
	var busy, frames, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed); err != nil {
		if IsBusyError(err) {
			return false, nil
		}
		return false, err
	}
	return busy == 0, nil
}
