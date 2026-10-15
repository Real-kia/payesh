package monitoring

import (
	"context"
	"fmt"
	"path/filepath"
	"syscall"
	"time"
)

// Databases created before incremental vacuum was the default keep
// auto_vacuum off, so space freed by retention is never returned and the file
// stays at its high-water mark. CompactIfNeeded converts such a database once
// (auto_vacuum=INCREMENTAL plus VACUUM). It is meant to run in the background
// after startup: readers keep working from their WAL snapshot while writes
// wait, and nodes buffer samples until it finishes.

const compactionTimeout = 20 * time.Minute

// compactionMinFreeBytes is a variable so tests can use small databases.
var compactionMinFreeBytes int64 = 64 << 20

// CompactionResult reports what CompactIfNeeded did.
type CompactionResult struct {
	Ran         bool
	Reason      string
	BeforeBytes int64
	AfterBytes  int64
}

// CompactIfNeeded converts a large, mostly free database with auto_vacuum off
// to incremental vacuum. It does nothing when the database already uses
// incremental vacuum, has little free space, or the disk lacks room for the
// rewrite (about twice the live data). A failure leaves the database as it
// was; the next start tries again.
func (s *Store) CompactIfNeeded(ctx context.Context) (CompactionResult, error) {
	if s.path == ":memory:" {
		return CompactionResult{Reason: "in-memory database"}, nil
	}
	var mode, pageSize, pageCount, freePages int64
	for query, target := range map[string]*int64{`PRAGMA auto_vacuum`: &mode, `PRAGMA page_size`: &pageSize, `PRAGMA page_count`: &pageCount, `PRAGMA freelist_count`: &freePages} {
		if err := s.readDB().QueryRowContext(ctx, query).Scan(target); err != nil {
			return CompactionResult{}, err
		}
	}
	if mode == 2 {
		return CompactionResult{Reason: "incremental vacuum already enabled"}, nil
	}
	free, total := freePages*pageSize, pageCount*pageSize
	if free < compactionMinFreeBytes || free*4 < total {
		return CompactionResult{Reason: "not enough free space to be worth compacting"}, nil
	}
	live := total - free
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(s.path), &fs); err != nil {
		return CompactionResult{}, err
	}
	if available := int64(fs.Bavail) * int64(fs.Bsize); available < 2*live+compactionMinFreeBytes {
		return CompactionResult{Reason: fmt.Sprintf("only %d MB free on disk; compaction needs about %d MB", available>>20, (2*live+compactionMinFreeBytes)>>20)}, nil
	}
	before, err := s.DatabaseBytes()
	if err != nil {
		return CompactionResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, compactionTimeout)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return CompactionResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return CompactionResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return CompactionResult{}, fmt.Errorf("vacuum: %w", err)
	}
	_, _ = conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	after, err := s.DatabaseBytes()
	if err != nil {
		return CompactionResult{}, err
	}
	return CompactionResult{Ran: true, BeforeBytes: before, AfterBytes: after}, nil
}
