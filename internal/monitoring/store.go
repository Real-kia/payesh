package monitoring

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Real-kia/payesh/internal/contracts"
	_ "modernc.org/sqlite"
)

const (
	MaxPageItems           = 200
	MaxAlertRules          = 20000
	MaxAlertRulesPerServer = 4096
	MaxLogBytes            = 1 << 20
	MaxPostProcessQueue    = 10000
	MaxTrafficAllowances   = 64
	MaxAuditEvents         = 10000
	// MetadataAge is deliberately longer than the raw metric API look-back.
	// Once an epoch has been inactive for this window and has no durable work
	// left, its replay/coverage metadata can be retired transactionally.
	DefaultMetadataAge = 31 * 24 * time.Hour
	// MetadataMaxRows is a safety valve for a fleet with many short-lived
	// collector epochs. Active and queued streams are never removed merely to
	// satisfy this cap.
	DefaultMetadataMaxRows = 10000
	// MaxForecastSamples bounds the internal raw-sample read used by the
	// traffic forecast adapter. It is intentionally larger than an API page
	// (a 15-second stream produces roughly 40k points in seven days), but still
	// finite so a damaged database cannot turn one forecast request into an
	// unbounded allocation.
	MaxForecastSamples = 50000
	// MaxRangeMetadataRows bounds fragmented gap/replay range authority across
	// the fleet. New disjoint ranges fail closed at capacity; merges that do not
	// grow the row set remain permitted.
	MaxRangeMetadataRows = DefaultMetadataMaxRows
)

const persistedTimeLayout = "2006-01-02T15:04:05.000000000Z"

// FormatPersistedTime returns a fixed-width UTC representation whose lexical
// ordering is identical to time ordering, including nanoseconds. API JSON may
// continue using RFC3339Nano; SQL keys and predicates must use this form.
func FormatPersistedTime(value time.Time) string {
	return value.UTC().Format(persistedTimeLayout)
}

var ErrStoragePressure = errors.New("managed storage budget is full; new history writes are paused")
var ErrRollupsPending = errors.New("derived metric rollups are pending; retry later")
var ErrTrafficAllowanceLimit = errors.New("traffic allowance count exceeds bound")
var ErrAlertRuleLimit = errors.New("alert rule count exceeds durable bound")

// ErrCollectorEpochAuthorityFull is fail-closed: the bounded permanent
// retirement authority has no room for another opaque epoch identity. Known
// live epochs remain writable, but a new identity is rejected until an
// operator raises the configured metadata bound.
var ErrCollectorEpochAuthorityFull = errors.New("collector epoch retirement authority is full; unseen epochs are paused")
var ErrRangeMetadataFull = errors.New("collector range metadata authority is full; new disjoint ranges are paused")
var memoryStoreID uint64

type StoreOptions struct {
	MaxBytes     int64
	ManagedPaths []string
}

type Store struct {
	db           *sql.DB
	path         string
	maxBytes     int64
	managedPaths []string
	observerMu   sync.RWMutex
	observer     func(context.Context, []contracts.MetricSample) error
}

// SetIngestionObserver registers the bounded post-commit package hook. The
// observer receives newly inserted samples, and may receive an empty slice
// when a coverage-gap-only commit unlocks samples already waiting in the
// durable post-process queue. Raw durability and node acknowledgements do not
// depend on derived alert/traffic processing.
func (s *Store) SetIngestionObserver(observer func(context.Context, []contracts.MetricSample) error) {
	if s == nil {
		return
	}
	s.observerMu.Lock()
	s.observer = observer
	s.observerMu.Unlock()
}

// ListPendingPostProcessSamples returns accepted samples whose derived work
// has not been acknowledged by the package observer yet. The bounded page is
// deliberately small so a damaged queue cannot monopolize ingestion.
func (s *Store) ListPendingPostProcessSamples(ctx context.Context, limit int) ([]contracts.MetricSample, error) {
	return s.listPendingPostProcessSamples(ctx, limit, nil)
}

// ListPendingPostProcessSamplesSkipping returns a bounded page while omitting
// identities that are known to be blocked on a missing predecessor. This lets
// a deferred stream wait without starving ready streams behind it.
func (s *Store) ListPendingPostProcessSamplesSkipping(ctx context.Context, limit int, skipped map[string]struct{}) ([]contracts.MetricSample, error) {
	return s.listPendingPostProcessSamples(ctx, limit, skipped)
}

func (s *Store) listPendingPostProcessSamples(ctx context.Context, limit int, skipped map[string]struct{}) ([]contracts.MetricSample, error) {
	if limit < 1 || limit > MaxPageItems {
		return nil, errors.New("invalid post-process queue limit")
	}
	query := `SELECT sample_json FROM post_process_queue`
	args := make([]any, 0, 2)
	if len(skipped) > 0 {
		identities := make([]string, 0, len(skipped))
		for identity := range skipped {
			identities = append(identities, identity)
		}
		if len(identities) > 0 {
			encoded, err := json.Marshal(identities)
			if err != nil {
				return nil, err
			}
			// One JSON parameter avoids SQLite's expression-depth and variable
			// limits when the bounded queue contains thousands of blocked streams.
			query += ` WHERE (server_id || char(0) || collector_epoch) NOT IN (SELECT value FROM json_each(?))`
			args = append(args, string(encoded))
		}
	}
	query += ` ORDER BY length(sequence),sequence,server_id,collector_epoch LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]contracts.MetricSample, 0, limit)
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var sample contracts.MetricSample
		if err := json.Unmarshal([]byte(encoded), &sample); err != nil {
			return nil, errors.New("invalid post-process queue sample")
		}
		if err := sample.Validate(); err != nil {
			return nil, err
		}
		result = append(result, sample)
	}
	return result, rows.Err()
}

func (s *Store) AcknowledgePostProcessSample(ctx context.Context, sample contracts.MetricSample) error {
	if sample.ServerID == "" || sample.CollectorEpoch == "" {
		return errors.New("post-process sample identity is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM post_process_queue WHERE server_id=? AND collector_epoch=? AND sequence=?`, string(sample.ServerID), string(sample.CollectorEpoch), strconv.FormatUint(sample.Sequence, 10))
	return err
}

func (s *Store) CoverageGapCovers(ctx context.Context, serverID contracts.ServerID, epoch contracts.CollectorEpoch, from, to uint64) (bool, error) {
	if from > to {
		return true, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	covered, err := CoverageGapCoversTx(ctx, tx, serverID, epoch, from, to)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return covered, nil
}

// SequenceFrontierReady reports whether raw samples and explicit coverage
// gaps establish a contiguous stream from sequence zero through target.
func (s *Store) SequenceFrontierReady(ctx context.Context, serverID contracts.ServerID, epoch contracts.CollectorEpoch, target uint64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	frontier, maxPoint, err := ensureSequenceFrontierValueTx(ctx, tx, serverID, epoch)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return sequenceFrontierCovers(frontier, maxPoint, target), nil
}

// CollectorEpochHasPredecessorTx reports whether this server has ever used a
// different collector epoch. Sequence continuity deliberately restarts at
// zero inside each epoch, but traffic-counter continuity does not: the first
// point after a collector restart is only a new baseline and cannot prove
// what happened between the last old-epoch point and the first new one.
//
// The transaction form keeps that decision in the same commit that updates
// traffic usage and continuity.
func CollectorEpochHasPredecessorTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch) (bool, error) {
	if tx == nil {
		return false, errors.New("collector epoch transaction is required")
	}
	if serverID == "" || epoch == "" {
		return false, errors.New("server and collector epoch are required")
	}
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM collector_epoch_metadata WHERE server_id=? AND collector_epoch<>?
		UNION ALL
		SELECT 1 FROM collector_epoch_retirements WHERE server_id=? AND collector_epoch<>?
	)`, string(serverID), string(epoch), string(serverID), string(epoch)).Scan(&exists)
	return exists == 1, err
}

// CoverageGapCoversTx is the transaction-safe form of CoverageGapCovers used
// by traffic's exactly-once usage transaction. A contiguous all-coverage
// frontier plus an indexed existence check for raw samples is sufficient: if
// the requested range is covered and contains no raw samples, every sequence
// in that range is covered by an explicit (half-open) gap.
func CoverageGapCoversTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, from, to uint64) (bool, error) {
	if tx == nil {
		return false, errors.New("coverage transaction is required")
	}
	if from > to {
		return true, nil
	}
	frontier, maxPoint, err := ensureSequenceFrontierValueTx(ctx, tx, serverID, epoch)
	if err != nil {
		return false, err
	}
	if !sequenceFrontierCovers(frontier, maxPoint, to) {
		return false, nil
	}
	hasSample, err := metricSampleExistsInRangeTx(ctx, tx, serverID, epoch, from, to)
	if err != nil {
		return false, err
	}
	return !hasSample, nil
}

func sequenceFrontierCovers(frontier uint64, maxPoint bool, target uint64) bool {
	return target < frontier || (target == ^uint64(0) && frontier == target && maxPoint)
}

// ensureSequenceFrontierValueTx creates the durable frontier row on demand.
// When upgrading a database created before sequence_frontiers existed, the
// one-time rebuild streams ordered rows and keeps only the current frontier in
// memory; it never allocates one interval per retained sample.
func ensureSequenceFrontierValueTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch) (uint64, bool, error) {
	retired, err := collectorEpochRetiredTx(ctx, tx, serverID, epoch)
	if err != nil {
		return 0, false, err
	}
	if retired {
		return 0, false, errors.New("collector epoch is retired")
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO sequence_frontiers(server_id,collector_epoch,frontier,max_point) VALUES(?,?,?,0) ON CONFLICT DO NOTHING`, string(serverID), string(epoch), "0")
	if err != nil {
		return 0, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if inserted > 0 {
		// Ingestion touches epoch metadata with the authoritative receipt time
		// before advancing the frontier. Do not overwrite that timestamp with
		// wall-clock time here (which can make a queued stream appear newer than
		// a deliberately current gap-only epoch during retention GC). Direct
		// frontier creation still gets a last-seen marker when no metadata exists.
		var metadataExists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collector_epoch_metadata WHERE server_id=? AND collector_epoch=?)`, string(serverID), string(epoch)).Scan(&metadataExists); err != nil {
			return 0, false, err
		}
		if metadataExists == 0 {
			if err := touchCollectorEpochTx(ctx, tx, serverID, epoch, time.Now().UTC()); err != nil {
				return 0, false, err
			}
		}
		if err := rebuildSequenceFrontierTx(ctx, tx, serverID, epoch); err != nil {
			return 0, false, err
		}
	}
	var frontierText string
	var maxPoint int
	if err := tx.QueryRowContext(ctx, `SELECT frontier,max_point FROM sequence_frontiers WHERE server_id=? AND collector_epoch=?`, string(serverID), string(epoch)).Scan(&frontierText, &maxPoint); err != nil {
		return 0, false, err
	}
	frontier, err := strconv.ParseUint(frontierText, 10, 64)
	if err != nil {
		return 0, false, errors.New("invalid sequence frontier")
	}
	return frontier, maxPoint != 0, nil
}

// rebuildSequenceFrontierTx lazily initializes a frontier for a legacy stream.
// The UNION is ordered by canonical decimal sequence length/text, so the Go
// side can stop at the first uncovered point while retaining O(1) memory.
func rebuildSequenceFrontierTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch) error {
	rows, err := tx.QueryContext(ctx, `SELECT from_sequence,to_sequence,is_gap FROM (SELECT sequence AS from_sequence,sequence AS to_sequence,0 AS is_gap,length(sequence) AS from_len,length(sequence) AS to_len FROM metric_samples WHERE server_id=? AND collector_epoch=? UNION ALL SELECT from_sequence,to_sequence,1 AS is_gap,length(from_sequence) AS from_len,length(to_sequence) AS to_len FROM coverage_gaps WHERE server_id=? AND collector_epoch=?) ORDER BY from_len,from_sequence,is_gap,to_len,to_sequence`, string(serverID), string(epoch), string(serverID), string(epoch))
	if err != nil {
		return err
	}
	const maxSequence = ^uint64(0)
	frontier := uint64(0)
	maxPoint := false
	for rows.Next() {
		var fromText, toText string
		var isGap int
		if err := rows.Scan(&fromText, &toText, &isGap); err != nil {
			_ = rows.Close()
			return err
		}
		from, err := strconv.ParseUint(fromText, 10, 64)
		if err != nil {
			_ = rows.Close()
			return errors.New("invalid persisted sequence")
		}
		if isGap == 0 {
			if from == maxSequence {
				maxPoint = true
				continue
			}
			if from > frontier {
				break
			}
			if from+1 > frontier {
				frontier = from + 1
			}
			continue
		}
		to, err := strconv.ParseUint(toText, 10, 64)
		if err != nil || to <= from {
			_ = rows.Close()
			return errors.New("invalid persisted coverage gap")
		}
		if from > frontier {
			break
		}
		if to > frontier {
			frontier = to
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sequence_frontiers SET frontier=?,max_point=? WHERE server_id=? AND collector_epoch=?`, strconv.FormatUint(frontier, 10), boolToInt(maxPoint), string(serverID), string(epoch))
	return err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func metricSampleExistsInRangeTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, from, to uint64) (bool, error) {
	fromText := strconv.FormatUint(from, 10)
	toText := strconv.FormatUint(to, 10)
	fromLen, toLen := len(fromText), len(toText)
	query := `SELECT EXISTS(SELECT 1 FROM metric_samples WHERE server_id=? AND collector_epoch=? AND `
	args := []any{string(serverID), string(epoch)}
	if fromLen == toLen {
		query += `length(sequence)=? AND sequence>=? AND sequence<=?`
		args = append(args, fromLen, fromText, toText)
	} else {
		query += `((length(sequence)=? AND sequence>=?) OR (length(sequence)>? AND length(sequence)<?) OR (length(sequence)=? AND sequence<=?))`
		args = append(args, fromLen, fromText, fromLen, toLen, toLen, toText)
	}
	query += `)`
	var found int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&found); err != nil {
		return false, err
	}
	return found != 0, nil
}

func advanceSequenceFrontierTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch) error {
	frontier, maxPointPresent, err := ensureSequenceFrontierValueTx(ctx, tx, serverID, epoch)
	if err != nil {
		return err
	}
	maxPoint := boolToInt(maxPointPresent)
	var maxFound int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metric_samples WHERE server_id=? AND collector_epoch=? AND sequence=?)`, string(serverID), string(epoch), strconv.FormatUint(^uint64(0), 10)).Scan(&maxFound); err != nil {
		return err
	}
	maxPoint = maxFound
	for {
		if frontier == ^uint64(0) {
			break
		}
		frontierText := strconv.FormatUint(frontier, 10)
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metric_samples WHERE server_id=? AND collector_epoch=? AND sequence=?)`, string(serverID), string(epoch), frontierText).Scan(&found); err != nil {
			return err
		}
		if found != 0 {
			frontier++
			continue
		}
		var gapTo string
		frontierLen := len(frontierText)
		gapErr := tx.QueryRowContext(ctx, `SELECT to_sequence FROM coverage_gaps WHERE server_id=? AND collector_epoch=? AND (length(from_sequence) < ? OR (length(from_sequence)=? AND from_sequence <= ?)) AND (length(to_sequence) > ? OR (length(to_sequence)=? AND to_sequence > ?)) ORDER BY length(to_sequence) DESC,to_sequence DESC LIMIT 1`, string(serverID), string(epoch), frontierLen, frontierLen, frontierText, frontierLen, frontierLen, frontierText).Scan(&gapTo)
		if errors.Is(gapErr, sql.ErrNoRows) {
			break
		}
		if gapErr != nil {
			return gapErr
		}
		jump, parseErr := strconv.ParseUint(gapTo, 10, 64)
		if parseErr != nil || jump <= frontier {
			break
		}
		frontier = jump
	}
	_, err = tx.ExecContext(ctx, `UPDATE sequence_frontiers SET frontier=?,max_point=? WHERE server_id=? AND collector_epoch=?`, strconv.FormatUint(frontier, 10), maxPoint, string(serverID), string(epoch))
	return err
}

// WithTransaction runs a bounded store operation in one SQLite transaction.
// Package-specific adapters use this seam when a configuration change must
// commit its durable state, revision, and idempotency record together.
func (s *Store) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	if s == nil || fn == nil {
		return errors.New("store transaction callback is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// TrafficUsageAlreadyAppliedTx reports whether a sample/allowance charge was
// already committed. The exact ledger covers live/recent samples; compact
// inclusive tombstone ranges cover rows removed by retention.
func TrafficUsageAlreadyAppliedTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, sequence uint64, scope, direction string) (bool, error) {
	if tx == nil {
		return false, errors.New("usage transaction is required")
	}
	retired, err := collectorEpochRetiredTx(ctx, tx, serverID, epoch)
	if err != nil {
		return false, err
	}
	if retired {
		// A retired epoch is permanently rejected at ingestion. Keep this
		// defensive result idempotent for package callers that already hold a
		// queued/decoded sample: it must never create a new charge.
		return true, nil
	}
	sequenceText := strconv.FormatUint(sequence, 10)
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traffic_usage_ledger WHERE server_id=? AND collector_epoch=? AND sequence=? AND scope=? AND direction=?)`, string(serverID), string(epoch), sequenceText, scope, direction).Scan(&found); err != nil {
		return false, err
	}
	if found != 0 {
		return true, nil
	}
	sequenceLen := len(sequenceText)
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope=? AND direction=? AND (length(from_sequence) < ? OR (length(from_sequence)=? AND from_sequence <= ?)) AND (length(to_sequence) > ? OR (length(to_sequence)=? AND to_sequence >= ?)) )`, string(serverID), string(epoch), scope, direction, sequenceLen, sequenceLen, sequenceText, sequenceLen, sequenceLen, sequenceText).Scan(&found); err != nil {
		return false, err
	}
	return found != 0, nil
}

func metricSampleTombstonedTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, sequence uint64) (bool, error) {
	retired, err := collectorEpochRetiredTx(ctx, tx, serverID, epoch)
	if err != nil {
		return false, err
	}
	if retired {
		return true, nil
	}
	sequenceText := strconv.FormatUint(sequence, 10)
	sequenceLen := len(sequenceText)
	var found int
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metric_sample_tombstones WHERE server_id=? AND collector_epoch=? AND (length(from_sequence) < ? OR (length(from_sequence)=? AND from_sequence <= ?)) AND (length(to_sequence) > ? OR (length(to_sequence)=? AND to_sequence >= ?)))`, string(serverID), string(epoch), sequenceLen, sequenceLen, sequenceText, sequenceLen, sequenceLen, sequenceText).Scan(&found)
	return found != 0, err
}

type IngestResult struct {
	Inserted           int  `json:"inserted"`
	Duplicate          int  `json:"duplicate"`
	RollupsPending     bool `json:"rollups_pending,omitempty"`
	PostProcessPending bool `json:"post_process_pending,omitempty"`
	// InsertedSamples is an internal post-commit hook payload. It is excluded
	// from JSON responses so node acknowledgements remain small and stable.
	InsertedSamples []contracts.MetricSample `json:"-"`
}

type MetricPage struct {
	Samples        []contracts.MetricSample `json:"samples"`
	Rollups        []Rollup                 `json:"rollups"`
	Coverage       map[string]float64       `json:"coverage"`
	Gaps           []contracts.CoverageGap  `json:"gaps,omitempty"`
	NextCursor     string                   `json:"next_cursor,omitempty"`
	GapsNextCursor string                   `json:"gaps_next_cursor,omitempty"`
	Truncated      bool                     `json:"truncated,omitempty"`
}

type TrafficPage struct {
	Periods    []contracts.TrafficPeriod `json:"periods"`
	NextCursor string                    `json:"next_cursor,omitempty"`
	Truncated  bool                      `json:"truncated,omitempty"`
}

type LogSourcePage struct {
	Items      []LogSource `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
	Truncated  bool        `json:"truncated,omitempty"`
}

type ServerPage struct {
	Items      []contracts.Server `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
	Truncated  bool               `json:"truncated,omitempty"`
}

type LogEntry struct {
	ServerID  contracts.ServerID `json:"server_id,omitempty"`
	SourceID  string             `json:"source_id"`
	Cursor    string             `json:"cursor"`
	Timestamp time.Time          `json:"timestamp"`
	Severity  string             `json:"severity,omitempty"`
	Text      string             `json:"text"`
	Truncated bool               `json:"truncated,omitempty"`
	Redacted  bool               `json:"redacted,omitempty"`
}

type LogPage struct {
	Entries    []LogEntry `json:"entries"`
	NextCursor string     `json:"next_cursor,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
}

type LogSource struct {
	ServerID contracts.ServerID `json:"server_id,omitempty"`
	ID       string             `json:"id"`
	Label    string             `json:"label"`
	Path     string             `json:"-"`
}

type RetentionPolicy struct {
	FullResolutionAge time.Duration
	MinuteRollupAge   time.Duration
	HourRollupAge     time.Duration
	TrafficPeriodAge  time.Duration
	LogAge            time.Duration
	LogMaxBytes       int64
	BatchSize         int
	// MetadataAge bounds how long inactive collector-epoch replay/coverage
	// metadata is retained after its raw samples and processing work are gone.
	// The expiry is explicit because deleting this metadata shortens the
	// replay-protection window for a stopped/retired epoch.
	MetadataAge time.Duration
	// MetadataMaxRows is a fleet-wide safety valve for stale epoch metadata.
	// Streams with raw samples, a usage ledger row, or post-process queue work
	// are protected even when this cap is exceeded.
	MetadataMaxRows int
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		FullResolutionAge: 24 * time.Hour,
		MinuteRollupAge:   7 * 24 * time.Hour,
		HourRollupAge:     90 * 24 * time.Hour,
		TrafficPeriodAge:  13 * 31 * 24 * time.Hour,
		LogAge:            7 * 24 * time.Hour,
		LogMaxBytes:       100 << 20,
		BatchSize:         100,
		MetadataAge:       DefaultMetadataAge,
		MetadataMaxRows:   DefaultMetadataMaxRows,
	}
}

type PruneStats struct {
	MetricSamples  int64
	MinuteRollups  int64
	HourRollups    int64
	TrafficPeriods int64
	LogEntries     int64
	AlertHistory   int64
	Incidents      int64
}

func OpenStore(ctx context.Context, path string, options StoreOptions) (*Store, error) {
	if path == "" {
		path = "payesh.db"
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := path
	if path == ":memory:" {
		dsn = fmt.Sprintf("file:payesh-memory-%d?mode=memory&cache=shared&_pragma=busy_timeout(10000)&_txlock=immediate", atomic.AddUint64(&memoryStoreID, 1))
	} else if strings.HasPrefix(path, "file:") {
		if strings.Contains(path, "?") {
			dsn = path + "&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate"
		} else {
			dsn = path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate"
		}
	} else {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	managedPaths := append([]string(nil), options.ManagedPaths...)
	if len(managedPaths) == 0 && path != ":memory:" {
		// Installed service logs live here; callers may add deployment-specific
		// managed paths, but the default must not silently omit them.
		managedPaths = []string{"/var/log/payesh"}
	}
	store := &Store{db: db, path: path, maxBytes: options.MaxBytes, managedPaths: managedPaths}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("restrict database permissions: %w", err)
		}
	}
	return store, nil
}

func (s *Store) Close() error {
	if s.db != nil {
		_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	// Capture this before CREATE TABLE. Only databases that genuinely predate
	// temporal allowance history need a synthetic baseline; ordinary reopen
	// must never make a pre-creation sample retroactively chargeable.
	var allowanceVersionsPreexisting int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='traffic_allowance_versions'`).Scan(&allowanceVersionsPreexisting); err != nil {
		return fmt.Errorf("inspect traffic allowance version schema: %w", err)
	}
	const schema = `
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA auto_vacuum = INCREMENTAL;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL);
INSERT INTO schema_meta(version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_meta);
UPDATE schema_meta SET version=2 WHERE version < 2;
UPDATE schema_meta SET version=3 WHERE version < 3;
UPDATE schema_meta SET version=4 WHERE version < 4;
CREATE TABLE IF NOT EXISTS browser_auth_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  state_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS fleet_identity_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  state_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  state TEXT NOT NULL,
  revision TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  target_server_id TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  progress INTEGER NOT NULL DEFAULT 0,
  error_json TEXT,
  action_json TEXT,
  result_json TEXT,
  lease_token TEXT,
  lease_expires_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(kind, target_server_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS jobs_expiry ON jobs(expires_at, state, id);
CREATE TABLE IF NOT EXISTS job_cancellation_requests (
  job_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(job_id, idempotency_key),
  FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  address TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  architecture TEXT NOT NULL,
  platform TEXT NOT NULL,
  capabilities_json TEXT NOT NULL,
  version TEXT NOT NULL,
  last_heartbeat TEXT,
  connection_state TEXT NOT NULL,
  freshness_state TEXT NOT NULL,
  freshness_reason TEXT,
  configuration_revision TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS metric_samples (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  sequence TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  timestamp_uncertainty TEXT,
  values_json TEXT NOT NULL,
  counters_json TEXT,
  units_json TEXT,
  validity_json TEXT,
  PRIMARY KEY (server_id, collector_epoch, sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS metric_samples_time ON metric_samples(server_id, observed_at, sequence);
CREATE TABLE IF NOT EXISTS coverage_gaps (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  from_sequence TEXT NOT NULL,
  to_sequence TEXT NOT NULL,
  reason TEXT NOT NULL,
  from_observed_at TEXT,
  to_observed_at TEXT,
  PRIMARY KEY (server_id, collector_epoch, from_sequence, to_sequence, reason),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS coverage_gaps_frontier ON coverage_gaps(server_id, collector_epoch, length(from_sequence), from_sequence, length(to_sequence), to_sequence);
CREATE INDEX IF NOT EXISTS metric_samples_sequence_range ON metric_samples(server_id, collector_epoch, length(sequence), sequence);
CREATE TABLE IF NOT EXISTS metric_rollups (
  server_id TEXT NOT NULL,
  metric TEXT NOT NULL,
  bucket_start TEXT NOT NULL,
  bucket_seconds INTEGER NOT NULL,
  sample_count INTEGER NOT NULL,
  observed_seconds REAL NOT NULL,
  minimum REAL,
  maximum REAL,
  weighted_mean REAL,
  counter_delta TEXT,
  coverage TEXT NOT NULL,
  PRIMARY KEY (server_id, metric, bucket_start, bucket_seconds),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS metric_rollups_time ON metric_rollups(server_id, bucket_start, bucket_seconds);
CREATE TABLE IF NOT EXISTS rollup_rebuild_queue (
  server_id TEXT NOT NULL,
  bucket_start TEXT NOT NULL,
  bucket_seconds INTEGER NOT NULL,
  PRIMARY KEY (server_id, bucket_start, bucket_seconds),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS traffic_periods (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  period_start TEXT NOT NULL,
  period_end TEXT NOT NULL,
  timezone TEXT NOT NULL,
  allowance_bytes TEXT NOT NULL,
  direction TEXT NOT NULL,
  counted_bytes TEXT NOT NULL,
  continuity TEXT NOT NULL,
  interfaces_json TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (server_id, scope, period_start, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_periods_time ON traffic_periods(server_id, period_start, scope, direction);
-- Contiguous sequence frontier for O(1) post-commit readiness checks. frontier
-- is the first sequence not covered by a raw sample or explicit gap; max_point
-- records a durable sequence=MaxUint64 sample without overflowing frontier.
CREATE TABLE IF NOT EXISTS sequence_frontiers (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  frontier TEXT NOT NULL,
  max_point INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, collector_epoch),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Last-seen timestamps let retention retire metadata from collector epochs
-- that have stopped producing data. Epoch metadata is deliberately separate
-- from raw samples because samples are deleted on a shorter age policy.
CREATE TABLE IF NOT EXISTS collector_epoch_metadata (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS collector_epoch_metadata_last_seen ON collector_epoch_metadata(last_seen_at, server_id, collector_epoch);
-- Retired epochs are a compact authority for replay rejection. Range
-- tombstones may be removed only after this marker is committed; unlike the
-- age-bounded coverage metadata, these markers are retained so a late sample
-- can never reopen a charged usage identity.
CREATE TABLE IF NOT EXISTS collector_epoch_retirements (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  retired_at TEXT NOT NULL,
  PRIMARY KEY(server_id,collector_epoch),
  FOREIGN KEY(server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS collector_epoch_retirements_time ON collector_epoch_retirements(retired_at, server_id, collector_epoch);
-- Opaque epoch IDs cannot be summarized by a monotonic cutoff. This singleton
-- therefore bounds exact permanent retirement markers and records the
-- fail-closed state used to reject previously unseen epochs at capacity.
CREATE TABLE IF NOT EXISTS collector_epoch_retirement_authority (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  max_rows INTEGER NOT NULL,
  saturated INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO collector_epoch_retirement_authority(singleton,max_rows,saturated) VALUES(1,10000,0);
-- Exactly-once usage application key. The raw sample queue may be replayed
-- after an observer crash, but a server/epoch/sequence/allowance tuple can
-- charge its period only once.
CREATE TABLE IF NOT EXISTS traffic_usage_ledger (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  sequence TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, sequence, scope, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Compact, durable replay tombstones for ledger rows whose raw samples have
-- aged out. Ranges are inclusive and preserve exactly-once charging without
-- retaining one ledger row per sample forever.
CREATE TABLE IF NOT EXISTS traffic_usage_tombstones (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  from_sequence TEXT NOT NULL,
  to_sequence TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, scope, direction, from_sequence, to_sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_usage_tombstones_lookup ON traffic_usage_tombstones(server_id, collector_epoch, scope, direction, from_sequence);
-- Raw-sample retention tombstones prevent a late retransmission from being
-- accepted as a fresh sample (and charged under a newer allowance) after the
-- original detailed row has aged out. Bounds are inclusive.
CREATE TABLE IF NOT EXISTS metric_sample_tombstones (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  from_sequence TEXT NOT NULL,
  to_sequence TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, from_sequence, to_sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS metric_sample_tombstones_lookup ON metric_sample_tombstones(server_id, collector_epoch, from_sequence);
CREATE TABLE IF NOT EXISTS log_sources (
  server_id TEXT NOT NULL,
  id TEXT NOT NULL,
  label TEXT NOT NULL,
  path TEXT NOT NULL,
  PRIMARY KEY (server_id, id),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS log_entries (
  server_id TEXT NOT NULL,
  source_id TEXT NOT NULL,
  cursor TEXT NOT NULL,
  timestamp TEXT NOT NULL,
  severity TEXT,
  text TEXT NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  redacted INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, source_id, cursor),
  FOREIGN KEY (server_id, source_id) REFERENCES log_sources(server_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS log_entries_time ON log_entries(server_id, source_id, timestamp, cursor);
CREATE TABLE IF NOT EXISTS traffic_allowances (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  interfaces_json TEXT NOT NULL,
  allowance_bytes TEXT NOT NULL,
  reset_day INTEGER NOT NULL,
  timezone TEXT NOT NULL,
  warnings_json TEXT NOT NULL,
  PRIMARY KEY (server_id, scope, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Immutable configuration history makes delayed post-processing use the
-- allowance that was effective when a sample was accepted. The current table
-- remains the bounded owner-facing projection and CAS target.
CREATE TABLE IF NOT EXISTS traffic_allowance_versions (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  effective_at TEXT NOT NULL,
  interfaces_json TEXT NOT NULL,
  allowance_bytes TEXT NOT NULL,
  reset_day INTEGER NOT NULL,
  timezone TEXT NOT NULL,
  warnings_json TEXT NOT NULL,
  PRIMARY KEY (server_id, scope, direction, effective_at),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_allowance_versions_effective ON traffic_allowance_versions(server_id, scope, direction, effective_at);
-- A schedule edit is kept separately from the current allowance so a server
-- restart cannot accidentally apply the new reset boundary in the middle of
-- an already-counted calendar period. The row is removed atomically when its
-- effective boundary is reached.
CREATE TABLE IF NOT EXISTS traffic_allowance_changes (
  server_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  direction TEXT NOT NULL,
  effective_at TEXT NOT NULL,
  previous_allowance_json TEXT NOT NULL,
  proposed_allowance_json TEXT NOT NULL,
  PRIMARY KEY (server_id, scope, direction),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_allowance_changes_effective ON traffic_allowance_changes(effective_at, server_id);
CREATE TABLE IF NOT EXISTS traffic_allowance_requests (
  server_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, idempotency_key),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS traffic_allowance_requests_created ON traffic_allowance_requests(created_at);
CREATE TABLE IF NOT EXISTS alert_rules (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  expression TEXT NOT NULL,
  server_id TEXT,
  enabled INTEGER NOT NULL,
  duration_seconds INTEGER NOT NULL,
  recovery_threshold REAL,
  reminder_seconds INTEGER NOT NULL,
  group_key TEXT,
  idempotency_key TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  effective_at TEXT NOT NULL,
  disable_reason TEXT NOT NULL DEFAULT '',
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Raw samples are acknowledged before package-specific derived work runs.
-- Keep the exact accepted sample here so a crashed observer can replay it
-- without asking the node to resend (which would be deduplicated at ingest).
CREATE TABLE IF NOT EXISTS post_process_queue (
  server_id TEXT NOT NULL,
  collector_epoch TEXT NOT NULL,
  sequence TEXT NOT NULL,
  sample_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, collector_epoch, sequence),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS post_process_queue_created ON post_process_queue(created_at, server_id);
CREATE INDEX IF NOT EXISTS alert_rules_server ON alert_rules(server_id, id);
CREATE TABLE IF NOT EXISTS alert_states (
  id TEXT PRIMARY KEY,
  rule_id TEXT NOT NULL,
  server_id TEXT,
  state TEXT NOT NULL,
  pending_since TEXT,
  firing_since TEXT,
  recovered_at TEXT,
  last_observation TEXT,
  last_value REAL,
  last_notified_at TEXT,
  last_suppressed_at TEXT,
  pending_recovery_at TEXT,
  incident_id TEXT,
  precision_warning TEXT,
  revision INTEGER NOT NULL DEFAULT 0,
  UNIQUE(rule_id, server_id),
  FOREIGN KEY (rule_id) REFERENCES alert_rules(id) ON DELETE CASCADE,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS alert_states_server ON alert_states(server_id, state, id);
CREATE TABLE IF NOT EXISTS alert_history (
  id TEXT PRIMARY KEY,
  alert_id TEXT NOT NULL,
  server_id TEXT,
  state TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  reason TEXT,
  value REAL,
  incident_id TEXT,
  FOREIGN KEY (alert_id) REFERENCES alert_rules(id) ON DELETE CASCADE,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS alert_history_time ON alert_history(occurred_at, id);
CREATE TABLE IF NOT EXISTS maintenance_windows (
  id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  starts_at TEXT NOT NULL,
  ends_at TEXT NOT NULL,
  server_ids_json TEXT NOT NULL,
  reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS maintenance_windows_time ON maintenance_windows(starts_at, ends_at, id);
CREATE TABLE IF NOT EXISTS incidents (
  id TEXT PRIMARY KEY,
  server_id TEXT,
  group_key TEXT NOT NULL,
  state TEXT NOT NULL,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  summary TEXT NOT NULL,
  events_json TEXT NOT NULL,
  evidence_json TEXT NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS incidents_server_time ON incidents(server_id, started_at, id);
-- Durable per-server optional-module lifecycle (package 06). revision is a
-- compare-and-swap counter so concurrent install/enable/disable/remove
-- requests for the same module cannot race each other's state transition.
CREATE TABLE IF NOT EXISTS module_installations (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  error_json TEXT,
  PRIMARY KEY (server_id, module_id),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS module_installations_server ON module_installations(server_id, module_id);
-- Idempotent lifecycle-request results, mirroring traffic_allowance_requests:
-- a repeated Install/Enable/Disable/Remove click with the same key returns
-- the original outcome instead of running the action twice.
CREATE TABLE IF NOT EXISTS module_lifecycle_requests (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, module_id, idempotency_key),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS module_lifecycle_requests_created ON module_lifecycle_requests(created_at);
-- Shared control-policy record (package 08 CPU Controls today; package 09
-- Bandwidth Controls reuses this same table/shape). One row per server/
-- module/target: a second Apply for the same target is a revision-CAS
-- update to the same row, never a duplicate policy.
CREATE TABLE IF NOT EXISTS control_policies (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_name TEXT NOT NULL,
  kind TEXT NOT NULL,
  state TEXT NOT NULL,
  parameters_json TEXT NOT NULL DEFAULT '{}',
  revision INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  error_json TEXT,
  PRIMARY KEY (server_id, module_id, target_kind, target_name),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS control_policies_server ON control_policies(server_id, state);
CREATE TABLE IF NOT EXISTS control_policy_requests (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_name TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (server_id, module_id, target_kind, target_name, idempotency_key),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS control_policy_requests_created ON control_policy_requests(created_at);
-- Port Traffic's counter identity is not the same as its user-facing scope
-- name.  Keep the identity claim in SQLite so two server processes cannot
-- select the same kernel counter between a read and a later policy write.
CREATE TABLE IF NOT EXISTS control_policy_unique_keys (
  server_id TEXT NOT NULL,
  module_id TEXT NOT NULL,
  unique_key TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_name TEXT NOT NULL,
  PRIMARY KEY (server_id, module_id, unique_key),
  UNIQUE (server_id, module_id, target_kind, target_name),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS control_policy_unique_keys_target ON control_policy_unique_keys(server_id,module_id,target_kind,target_name);
-- Durable observations are deliberately separate from control policy
-- parameters: replacing a scope must not make an absolute nft counter look
-- like desired configuration or increase its revision.
CREATE TABLE IF NOT EXISTS port_traffic_observations (
  server_id TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  bytes TEXT NOT NULL,
  packets TEXT NOT NULL,
  generation TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  continuity TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (server_id, scope_id),
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
);
-- Redacted structured audit records. Policy transitions insert their audit
-- row in the same SQLite transaction as the state change.
CREATE TABLE IF NOT EXISTS audit_events (
  id TEXT PRIMARY KEY,
  occurred_at TEXT NOT NULL,
  actor_type TEXT NOT NULL,
  actor_id TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL DEFAULT '',
  result TEXT NOT NULL,
  revision INTEGER NOT NULL,
  redacted INTEGER NOT NULL DEFAULT 1,
  correlation_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_events_time ON audit_events(occurred_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_events_target ON audit_events(target_id, occurred_at DESC, id DESC);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite schema: %w", err)
	}
	// These columns were added after the initial durable-job checkpoint. Keep
	// startup compatible with existing stores instead of requiring a destructive
	// migration or silently dropping queued action payloads.
	for _, column := range []struct {
		name string
		ddl  string
	}{
		{name: "action_json", ddl: "ALTER TABLE jobs ADD COLUMN action_json TEXT"},
		{name: "result_json", ddl: "ALTER TABLE jobs ADD COLUMN result_json TEXT"},
		{name: "lease_token", ddl: "ALTER TABLE jobs ADD COLUMN lease_token TEXT"},
		{name: "lease_expires_at", ddl: "ALTER TABLE jobs ADD COLUMN lease_expires_at TEXT"},
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name=?`, column.name).Scan(&count); err != nil {
			return fmt.Errorf("inspect jobs schema: %w", err)
		}
		if count == 0 {
			if _, err := s.db.ExecContext(ctx, column.ddl); err != nil {
				return fmt.Errorf("migrate jobs schema: %w", err)
			}
		}
	}
	var disableReasonColumn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_rules') WHERE name='disable_reason'`).Scan(&disableReasonColumn); err != nil {
		return fmt.Errorf("inspect alert rule schema: %w", err)
	}
	if disableReasonColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE alert_rules ADD COLUMN disable_reason TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate alert rule schema: %w", err)
		}
	}
	var effectiveAtColumn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_rules') WHERE name='effective_at'`).Scan(&effectiveAtColumn); err != nil {
		return fmt.Errorf("inspect alert rule effective timestamp schema: %w", err)
	}
	if effectiveAtColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE alert_rules ADD COLUMN effective_at TEXT`); err != nil {
			return fmt.Errorf("migrate alert rule effective timestamp schema: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE alert_rules SET effective_at=created_at WHERE effective_at IS NULL OR effective_at=''`); err != nil {
			return fmt.Errorf("backfill alert rule effective timestamp: %w", err)
		}
	}
	var suppressedColumn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_states') WHERE name='last_suppressed_at'`).Scan(&suppressedColumn); err != nil {
		return fmt.Errorf("inspect alert state schema: %w", err)
	}
	if suppressedColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE alert_states ADD COLUMN last_suppressed_at TEXT`); err != nil {
			return fmt.Errorf("migrate alert state schema: %w", err)
		}
	}
	var pendingRecoveryColumn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_states') WHERE name='pending_recovery_at'`).Scan(&pendingRecoveryColumn); err != nil {
		return fmt.Errorf("inspect alert state pending recovery schema: %w", err)
	}
	if pendingRecoveryColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE alert_states ADD COLUMN pending_recovery_at TEXT`); err != nil {
			return fmt.Errorf("migrate alert state pending recovery schema: %w", err)
		}
	}
	var alertStateRevisionColumn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('alert_states') WHERE name='revision'`).Scan(&alertStateRevisionColumn); err != nil {
		return fmt.Errorf("inspect alert state revision schema: %w", err)
	}
	if alertStateRevisionColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE alert_states ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate alert state revision schema: %w", err)
		}
	}
	var serverAddressColumn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('servers') WHERE name='address'`).Scan(&serverAddressColumn); err != nil {
		return fmt.Errorf("inspect server address schema: %w", err)
	}
	if serverAddressColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE servers ADD COLUMN address TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate server address schema: %w", err)
		}
	}
	var schemaVersion int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_meta LIMIT 1`).Scan(&schemaVersion); err != nil {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	if schemaVersion < 5 {
		if err := s.migratePersistedTimes(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE schema_meta SET version=5 WHERE version<5`); err != nil {
			return fmt.Errorf("record canonical timestamp migration: %w", err)
		}
	}
	var periodInterfacesColumn int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('traffic_periods') WHERE name='interfaces_json'`).Scan(&periodInterfacesColumn); err != nil {
		return fmt.Errorf("inspect traffic period schema: %w", err)
	}
	if periodInterfacesColumn == 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE traffic_periods ADD COLUMN interfaces_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate traffic period schema: %w", err)
		}
		// A legacy database has no way to reconstruct interface selections for
		// closed historical rows. The currently active row can still be
		// recovered from the authoritative allowance without relabelling every
		// historical period with today's configuration.
		now := FormatPersistedTime(time.Now())
		if _, err := s.db.ExecContext(ctx, `UPDATE traffic_periods SET interfaces_json=(SELECT interfaces_json FROM traffic_allowances a WHERE a.server_id=traffic_periods.server_id AND a.scope=traffic_periods.scope AND a.direction=traffic_periods.direction) WHERE interfaces_json='[]' AND period_start <= ? AND period_end > ? AND EXISTS(SELECT 1 FROM traffic_allowances a WHERE a.server_id=traffic_periods.server_id AND a.scope=traffic_periods.scope AND a.direction=traffic_periods.direction)`, now, now); err != nil {
			return fmt.Errorf("backfill active traffic period interfaces: %w", err)
		}
	}
	// Older Checkpoint A databases were created before temporal gap bounds
	// existed. ALTER TABLE is kept separate because SQLite has no portable
	// ADD COLUMN IF NOT EXISTS form.
	for _, column := range []string{"from_observed_at", "to_observed_at"} {
		var present int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('coverage_gaps') WHERE name=?`, column).Scan(&present); err != nil {
			return fmt.Errorf("inspect coverage gap schema: %w", err)
		}
		if present == 0 {
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE coverage_gaps ADD COLUMN `+column+` TEXT`); err != nil {
				return fmt.Errorf("migrate coverage gap schema: %w", err)
			}
		}
	}
	// Schedule-transition state was introduced after the initial package-05
	// schema. Existing databases may already have the table with an earlier
	// column layout; add the durable JSON column before traffic consumers read
	// it. (New databases create the final layout above.)
	var changesTable int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='traffic_allowance_changes'`).Scan(&changesTable); err != nil {
		return fmt.Errorf("inspect traffic allowance change schema: %w", err)
	}
	if changesTable > 0 {
		var previousJSON, proposedJSON int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('traffic_allowance_changes') WHERE name='previous_allowance_json'`).Scan(&previousJSON); err != nil {
			return fmt.Errorf("inspect traffic allowance change columns: %w", err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('traffic_allowance_changes') WHERE name='proposed_allowance_json'`).Scan(&proposedJSON); err != nil {
			return fmt.Errorf("inspect traffic allowance proposed column: %w", err)
		}
		if proposedJSON == 0 {
			return errors.New("traffic allowance migration cannot reconstruct missing proposed schedule column")
		}
		if previousJSON == 0 {
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE traffic_allowance_changes ADD COLUMN previous_allowance_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
				return fmt.Errorf("migrate traffic allowance change columns: %w", err)
			}
		}
		var invalidPrevious, invalidProposed int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_allowance_changes WHERE previous_allowance_json='{}'`).Scan(&invalidPrevious); err != nil {
			return fmt.Errorf("validate traffic allowance history: %w", err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_allowance_changes WHERE proposed_allowance_json='{}'`).Scan(&invalidProposed); err != nil {
			return fmt.Errorf("validate proposed traffic allowance history: %w", err)
		}
		if invalidPrevious != 0 || invalidProposed != 0 {
			return fmt.Errorf("traffic allowance migration cannot reconstruct schedule rows (previous=%d proposed=%d)", invalidPrevious, invalidProposed)
		}
	}
	// Only a database whose version-history table was absent before this
	// migration needs the legacy baseline. Running this on every startup would
	// retroactively authorize samples received before a newly-created policy.
	if allowanceVersionsPreexisting == 0 {
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO traffic_allowance_versions(server_id,scope,direction,effective_at,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json)
SELECT server_id,scope,direction,?,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json FROM traffic_allowances`, FormatPersistedTime(time.Time{})); err != nil {
			return fmt.Errorf("backfill traffic allowance versions: %w", err)
		}
	}
	if err := s.backfillCollectorEpochMetadata(ctx); err != nil {
		return err
	}
	return nil
}

type persistedTimeColumn struct {
	table  string
	column string
}

// migratePersistedTimes converts every SQL timestamp to one fixed-width UTC
// encoding. SQLite can then compare and index the TEXT values exactly down to
// one nanosecond. The transaction intentionally fails closed on a primary-key
// collision (for example two legacy offset spellings of the same key instant)
// instead of silently discarding either durable row.
func (s *Store) migratePersistedTimes(ctx context.Context) error {
	columns := []persistedTimeColumn{
		{"servers", "last_heartbeat"},
		{"metric_samples", "observed_at"}, {"metric_samples", "received_at"},
		{"coverage_gaps", "from_observed_at"}, {"coverage_gaps", "to_observed_at"},
		{"metric_rollups", "bucket_start"}, {"rollup_rebuild_queue", "bucket_start"},
		{"traffic_periods", "period_start"}, {"traffic_periods", "period_end"},
		{"collector_epoch_metadata", "first_seen_at"}, {"collector_epoch_metadata", "last_seen_at"},
		{"collector_epoch_retirements", "retired_at"},
		{"traffic_allowance_versions", "effective_at"}, {"traffic_allowance_changes", "effective_at"},
		{"traffic_allowance_requests", "created_at"}, {"post_process_queue", "created_at"},
		{"log_entries", "timestamp"},
		{"alert_rules", "created_at"}, {"alert_rules", "effective_at"},
		{"alert_states", "pending_since"}, {"alert_states", "firing_since"},
		{"alert_states", "recovered_at"}, {"alert_states", "last_observation"},
		{"alert_states", "last_notified_at"}, {"alert_states", "last_suppressed_at"},
		{"alert_states", "pending_recovery_at"},
		{"alert_history", "occurred_at"},
		{"maintenance_windows", "starts_at"}, {"maintenance_windows", "ends_at"},
		{"incidents", "started_at"}, {"incidents", "ended_at"},
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin canonical timestamp migration: %w", err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	for _, target := range columns {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, target.table, target.column).Scan(&present); err != nil {
			return rollback(fmt.Errorf("inspect %s.%s timestamp column: %w", target.table, target.column, err))
		}
		if present == 0 {
			continue
		}
		var after int64
		for {
			query := `SELECT rowid,` + target.column + ` FROM ` + target.table + ` WHERE rowid>? AND ` + target.column + ` IS NOT NULL AND ` + target.column + `<>'' ORDER BY rowid LIMIT ?`
			rows, err := tx.QueryContext(ctx, query, after, MaxPageItems)
			if err != nil {
				return rollback(fmt.Errorf("read %s.%s timestamps: %w", target.table, target.column, err))
			}
			type timestampRow struct {
				rowID int64
				value string
			}
			batch := make([]timestampRow, 0, MaxPageItems)
			for rows.Next() {
				var row timestampRow
				if err := rows.Scan(&row.rowID, &row.value); err != nil {
					_ = rows.Close()
					return rollback(fmt.Errorf("scan %s.%s timestamp: %w", target.table, target.column, err))
				}
				batch = append(batch, row)
			}
			if err := rows.Close(); err != nil {
				return rollback(fmt.Errorf("close %s.%s timestamp rows: %w", target.table, target.column, err))
			}
			if len(batch) == 0 {
				break
			}
			for _, row := range batch {
				parsed, err := time.Parse(time.RFC3339Nano, row.value)
				if err != nil {
					return rollback(fmt.Errorf("parse %s.%s timestamp %q: %w", target.table, target.column, row.value, err))
				}
				canonical := FormatPersistedTime(parsed)
				if canonical != row.value {
					update := `UPDATE ` + target.table + ` SET ` + target.column + `=? WHERE rowid=?`
					if _, err := tx.ExecContext(ctx, update, canonical, row.rowID); err != nil {
						return rollback(fmt.Errorf("canonicalize %s.%s timestamp: %w", target.table, target.column, err))
					}
				}
				after = row.rowID
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit canonical timestamp migration: %w", err)
	}
	return nil
}

// backfillCollectorEpochMetadata seeds the last-seen table for databases
// created before epoch metadata was introduced. Legacy rows are treated as
// newly observed so an upgrade never retires replay/coverage state
// immediately; normal ingestion updates the timestamp from then on.
func (s *Store) backfillCollectorEpochMetadata(ctx context.Context) error {
	now := FormatPersistedTime(time.Now())
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO collector_epoch_metadata(server_id,collector_epoch,first_seen_at,last_seen_at)
SELECT server_id,collector_epoch,?,? FROM (
  SELECT DISTINCT server_id,collector_epoch FROM metric_samples
  UNION SELECT DISTINCT server_id,collector_epoch FROM coverage_gaps
  UNION SELECT DISTINCT server_id,collector_epoch FROM sequence_frontiers
  UNION SELECT DISTINCT server_id,collector_epoch FROM traffic_usage_ledger
  UNION SELECT DISTINCT server_id,collector_epoch FROM traffic_usage_tombstones
  UNION SELECT DISTINCT server_id,collector_epoch FROM metric_sample_tombstones
  UNION SELECT DISTINCT server_id,collector_epoch FROM post_process_queue
)`, now, now)
	if err != nil {
		return fmt.Errorf("backfill collector epoch metadata: %w", err)
	}
	return nil
}

func touchCollectorEpochTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, seenAt time.Time) error {
	if tx == nil {
		return errors.New("collector epoch transaction is required")
	}
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	}
	seen := FormatPersistedTime(seenAt)
	_, err := tx.ExecContext(ctx, `INSERT INTO collector_epoch_metadata(server_id,collector_epoch,first_seen_at,last_seen_at) VALUES(?,?,?,?) ON CONFLICT(server_id,collector_epoch) DO UPDATE SET first_seen_at=CASE WHEN collector_epoch_metadata.first_seen_at < excluded.first_seen_at THEN collector_epoch_metadata.first_seen_at ELSE excluded.first_seen_at END,last_seen_at=CASE WHEN collector_epoch_metadata.last_seen_at > excluded.last_seen_at THEN collector_epoch_metadata.last_seen_at ELSE excluded.last_seen_at END`, string(serverID), string(epoch), seen, seen)
	return err
}

func collectorEpochRetiredTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch) (bool, error) {
	retired, blocked, err := collectorEpochAuthorityStatusTx(ctx, tx, serverID, epoch)
	return retired || blocked, err
}

func collectorEpochAuthorityStatusTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch) (retired, unseenBlocked bool, err error) {
	if tx == nil {
		return false, false, errors.New("collector epoch transaction is required")
	}
	var exact, saturated int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collector_epoch_retirements WHERE server_id=? AND collector_epoch=?),(SELECT saturated FROM collector_epoch_retirement_authority WHERE singleton=1)`, string(serverID), string(epoch)).Scan(&exact, &saturated); err != nil {
		return false, false, err
	}
	if exact != 0 {
		return true, false, nil
	}
	if saturated == 0 {
		return false, false, nil
	}
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM collector_epoch_metadata WHERE server_id=? AND collector_epoch=?)`, string(serverID), string(epoch)).Scan(&known); err != nil {
		return false, false, err
	}
	return false, known == 0, nil
}

func (s *Store) ensureWritable(ctx context.Context) error {
	if s.maxBytes <= 0 || s.path == ":memory:" {
		return nil
	}
	usage, err := s.StorageBytes()
	if err != nil {
		return err
	}
	threshold := s.maxBytes
	// Begin bounded eviction before the hard ceiling for normal installations.
	// Tiny test budgets are intentionally treated as hard ceilings so callers
	// can exercise derived-write pressure without deleting their fixture data.
	if s.maxBytes >= 8<<20 {
		threshold = s.maxBytes * 9 / 10
	}
	if usage < threshold {
		return nil
	}
	if s.maxBytes < 8<<20 {
		return ErrStoragePressure
	}
	if err := s.evictForStorage(ctx); err != nil {
		return err
	}
	usage, err = s.StorageBytes()
	if err != nil {
		return err
	}
	if usage >= s.maxBytes {
		return ErrStoragePressure
	}
	return nil
}

// evictForStorage shortens history before the hard managed-data ceiling. It
// never deletes server identity or traffic-period state. SQLite pages are
// reclaimed through WAL checkpointing and incremental vacuum; no full VACUUM
// is run on the write path.
func (s *Store) evictForStorage(ctx context.Context) error {
	for attempt := 0; attempt < 20; attempt++ {
		usage, err := s.StorageBytes()
		if err != nil {
			return err
		}
		if usage < s.maxBytes*85/100 {
			return nil
		}
		cutoff := FormatPersistedTime(time.Now().Add(time.Minute))
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		selection, err := collectRetentionGaps(ctx, tx, cutoff, MaxPageItems)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		deleted := int64(0)
		if len(selection.rowIDs) > 0 {
			placeholders := make([]string, len(selection.rowIDs))
			args := make([]any, len(selection.rowIDs))
			for i, rowID := range selection.rowIDs {
				placeholders[i], args[i] = "?", rowID
			}
			result, execErr := tx.ExecContext(ctx, `DELETE FROM metric_samples WHERE rowid IN (`+strings.Join(placeholders, ",")+")", args...)
			if execErr != nil {
				_ = tx.Rollback()
				return execErr
			}
			deleted, _ = result.RowsAffected()
			for _, gap := range selection.gaps {
				if execErr := insertCoverageGapTx(ctx, tx, gap.serverID, gap.epoch, gap.from, gap.to, "retention", gap.fromObserved, gap.toObserved); execErr != nil {
					_ = tx.Rollback()
					return execErr
				}
			}
			if err := insertRetentionTombstonesTx(ctx, tx, selection.tombstones); err != nil {
				_ = tx.Rollback()
				return err
			}
			for stream := range selection.frontierStreams {
				if err := advanceSequenceFrontierTx(ctx, tx, stream.serverID, stream.epoch); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		if deleted == 0 {
			// If detailed samples are already gone, remove the oldest derived
			// history and logs before giving up. Protected traffic periods are not
			// part of this fallback.
			for _, table := range []string{"metric_rollups", "log_entries"} {
				result, execErr := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid IN (SELECT rowid FROM `+table+` ORDER BY rowid ASC LIMIT ?)`, MaxPageItems)
				if execErr != nil {
					_ = tx.Rollback()
					return execErr
				}
				count, _ := result.RowsAffected()
				deleted += count
				if deleted > 0 {
					break
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if deleted == 0 {
			return nil
		}
		_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		_, _ = s.db.ExecContext(ctx, `PRAGMA incremental_vacuum(256)`)
	}
	return nil
}

// ensureDerivedWritable reserves headroom for rollups without evicting raw
// samples that were just durably accepted. History eviction belongs before the
// ingestion transaction; a derived retry must never make that raw commit
// disappear.
func (s *Store) ensureDerivedWritable(ctx context.Context) error {
	if s.maxBytes <= 0 || s.path == ":memory:" {
		return nil
	}
	usage, err := s.StorageBytes()
	if err != nil {
		return err
	}
	if usage >= s.maxBytes-4096 {
		return ErrStoragePressure
	}
	return nil
}

func (s *Store) StorageBytes() (int64, error) {
	if s.path == ":memory:" {
		return 0, nil
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Stat(s.path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	for _, root := range s.managedPaths {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) {
					return filepath.SkipDir
				}
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type().IsRegular() {
				info, statErr := entry.Info()
				if statErr != nil {
					return statErr
				}
				total += info.Size()
			}
			return nil
		})
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

const (
	serverFreshAfter        = 45 * time.Second
	serverDisconnectedAfter = 90 * time.Second
)

// RefreshServerStates derives connection/freshness from the last durable
// heartbeat. It is safe to call before reads and from the retention worker.
func (s *Store) RefreshServerStates(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	stale := FormatPersistedTime(now.Add(-serverFreshAfter))
	disconnected := FormatPersistedTime(now.Add(-serverDisconnectedAfter))
	_, err := s.db.ExecContext(ctx, `UPDATE servers SET
connection_state=CASE WHEN connection_state='revoked' THEN 'revoked' WHEN last_heartbeat IS NULL THEN 'never-connected' WHEN last_heartbeat <= ? THEN 'disconnected' ELSE 'connected' END,
freshness_state=CASE WHEN last_heartbeat IS NULL THEN 'unknown' WHEN last_heartbeat <= ? THEN 'stale' ELSE 'fresh' END,
freshness_reason=CASE WHEN last_heartbeat IS NULL THEN 'no-heartbeat' WHEN last_heartbeat <= ? THEN 'heartbeat-timeout' ELSE NULL END`, disconnected, stale, stale)
	return err
}

func (s *Store) UpsertServer(ctx context.Context, server contracts.Server) error {
	if err := validateServer(server); err != nil {
		return err
	}
	capabilities, err := json.Marshal(server.Capabilities)
	if err != nil {
		return err
	}
	var heartbeat any
	if server.LastHeartbeat != nil {
		heartbeat = FormatPersistedTime(*server.LastHeartbeat)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO servers(id,name,address,role,architecture,platform,capabilities_json,version,last_heartbeat,connection_state,freshness_state,freshness_reason,configuration_revision)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,address=CASE WHEN excluded.address='' THEN servers.address ELSE excluded.address END,role=excluded.role,architecture=excluded.architecture,platform=excluded.platform,capabilities_json=excluded.capabilities_json,version=excluded.version,last_heartbeat=excluded.last_heartbeat,connection_state=excluded.connection_state,freshness_state=excluded.freshness_state,freshness_reason=excluded.freshness_reason,configuration_revision=excluded.configuration_revision`,
		string(server.ID), server.Name, server.Address, server.Role, server.Architecture, server.Platform, string(capabilities), server.Version, heartbeat, server.ConnectionState, server.FreshnessState, server.FreshnessReason, strconv.FormatUint(server.ConfigurationRevision, 10))
	return err
}

// EnsureServer creates the local server record once and preserves all durable
// state when an agent restarts. Service bootstrap uses this instead of
// UpsertServer so labels, revisions, freshness, and capabilities are not
// reset on every process start.
func (s *Store) EnsureServer(ctx context.Context, server contracts.Server) error {
	if err := validateServer(server); err != nil {
		return err
	}
	capabilities, err := json.Marshal(server.Capabilities)
	if err != nil {
		return err
	}
	var heartbeat any
	if server.LastHeartbeat != nil {
		heartbeat = FormatPersistedTime(*server.LastHeartbeat)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO servers(id,name,address,role,architecture,platform,capabilities_json,version,last_heartbeat,connection_state,freshness_state,freshness_reason,configuration_revision)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		string(server.ID), server.Name, server.Address, server.Role, server.Architecture, server.Platform, string(capabilities), server.Version, heartbeat, server.ConnectionState, server.FreshnessState, server.FreshnessReason, strconv.FormatUint(server.ConfigurationRevision, 10))
	return err
}

// TouchServer records a successful local ingestion without changing owner or
// configuration fields. It is intentionally a small, non-history update.
func (s *Store) TouchServer(ctx context.Context, serverID contracts.ServerID, receivedAt time.Time) error {
	if serverID == "" || receivedAt.IsZero() {
		return errors.New("server_id and received_at are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE servers SET last_heartbeat=?,connection_state='connected',freshness_state='fresh',freshness_reason=NULL WHERE id=?`, FormatPersistedTime(receivedAt), string(serverID))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateServerHello updates the server inventory (architecture, platform, capabilities, version)
// and marks it connected when a node performs its transport handshake.
func (s *Store) UpdateServerHello(ctx context.Context, serverID contracts.ServerID, hello contracts.Hello, receivedAt time.Time) error {
	if serverID == "" || receivedAt.IsZero() {
		return errors.New("server_id and received_at are required")
	}
	caps := append([]string(nil), hello.Capabilities...)
	if len(caps) == 0 {
		caps = []string{"metrics", "traffic"}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE servers SET
		last_heartbeat=?,
		connection_state='connected',
		freshness_state='fresh',
		freshness_reason=NULL,
		architecture=CASE WHEN ? != '' AND ? != 'unknown' THEN ? ELSE architecture END,
		platform=CASE WHEN ? != '' AND ? != 'unknown' THEN ? ELSE platform END,
		version=CASE WHEN ? != '' THEN ? ELSE version END,
		capabilities_json=?
		WHERE id=?`,
		FormatPersistedTime(receivedAt),
		hello.Architecture, hello.Architecture, hello.Architecture,
		hello.Platform, hello.Platform, hello.Platform,
		hello.Version, hello.Version,
		string(capsJSON),
		string(serverID),
	)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AdvanceServerConfigurationRevision conditionally advances the owner-facing
// configuration revision. Mutating APIs use this compare-and-swap boundary so
// a stale client cannot silently overwrite a newer configuration.
func (s *Store) AdvanceServerConfigurationRevision(ctx context.Context, serverID contracts.ServerID, expected uint64) (uint64, error) {
	if !validStoreServerID(serverID) {
		return 0, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if expected == ^uint64(0) {
		return 0, errors.New("configuration revision is exhausted")
	}
	next := expected + 1
	result, err := s.db.ExecContext(ctx, `UPDATE servers SET configuration_revision=? WHERE id=? AND configuration_revision=?`, strconv.FormatUint(next, 10), string(serverID), strconv.FormatUint(expected, 10))
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed != 1 {
		return 0, errors.New("configuration_revision_conflict")
	}
	return next, nil
}

// MarkServerRevoked records an identity revocation immediately. Refreshing
// heartbeat-derived state preserves this terminal status until an explicit
// recovery certificate successfully reconnects and touches the server.
func (s *Store) MarkServerRevoked(ctx context.Context, serverID contracts.ServerID) error {
	if !validStoreServerID(serverID) {
		return errors.New("server_id must be a bounded URL-safe identifier")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE servers SET connection_state='revoked',freshness_state='stale',freshness_reason='identity-revoked' WHERE id=?`, string(serverID))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// IsServerUnreachable exposes the durable fleet state to the alert engine so
// outage grouping survives an alert-engine restart. "never-connected" is not
// treated as an outage: a newly enrolled node has no evidence of failure yet.
func (s *Store) IsServerUnreachable(ctx context.Context, serverID contracts.ServerID) (bool, error) {
	if !validStoreServerID(serverID) {
		return false, errors.New("server_id must be a bounded URL-safe identifier")
	}
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT connection_state FROM servers WHERE id=?`, string(serverID)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == "disconnected" || state == "revoked", nil
}

func (s *Store) ListServers(ctx context.Context, limit int) ([]contracts.Server, error) {
	page, err := s.QueryServerPage(ctx, limit, "")
	return page.Items, err
}

func (s *Store) GetServer(ctx context.Context, serverID contracts.ServerID) (contracts.Server, bool, error) {
	if len(serverID) < 16 || len(serverID) > 128 || !isSafeServerID(string(serverID)) {
		return contracts.Server{}, false, errors.New("server id must be a bounded URL-safe identifier")
	}
	row := s.db.QueryRowContext(ctx, `SELECT id,name,address,role,architecture,platform,capabilities_json,version,last_heartbeat,connection_state,freshness_state,COALESCE(freshness_reason,''),configuration_revision FROM servers WHERE id=?`, string(serverID))
	server, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.Server{}, false, nil
	}
	if err != nil {
		return contracts.Server{}, false, err
	}
	return server, true, nil
}

// DeleteServer permanently removes a managed node and its cascade-owned data.
// The running hub/standalone identity requires its dedicated uninstall flow.
func (s *Store) DeleteServer(ctx context.Context, serverID contracts.ServerID, expectedRevision uint64) error {
	if !validStoreServerID(serverID) {
		return errors.New("server_id must be a bounded URL-safe identifier")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM servers WHERE id=? AND role='node' AND configuration_revision=?`, string(serverID), strconv.FormatUint(expectedRevision, 10))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		return nil
	}
	server, found, err := s.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	if !found {
		return sql.ErrNoRows
	}
	if server.Role != "node" {
		return errors.New("server_role_not_deletable")
	}
	return errors.New("configuration_revision_conflict")
}

type serverCursor struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

func (s *Store) QueryServerPage(ctx context.Context, limit int, cursor string) (ServerPage, error) {
	limit = clampLimit(limit)
	decoded, err := decodeServerCursor(cursor)
	if err != nil {
		return ServerPage{}, err
	}
	query := `SELECT id,name,address,role,architecture,platform,capabilities_json,version,last_heartbeat,connection_state,freshness_state,COALESCE(freshness_reason,''),configuration_revision FROM servers`
	args := make([]any, 0, 3)
	if decoded.Name != "" {
		query += ` WHERE (name > ? OR (name = ? AND id > ?))`
		args = append(args, decoded.Name, decoded.Name, decoded.ID)
	}
	query += ` ORDER BY name ASC, id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ServerPage{}, err
	}
	defer rows.Close()
	page := ServerPage{Items: make([]contracts.Server, 0, limit)}
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return ServerPage{}, err
		}
		if len(page.Items) == limit {
			page.Truncated = true
			last := page.Items[len(page.Items)-1]
			page.NextCursor = encodeServerCursor(serverCursor{Name: last.Name, ID: string(last.ID)})
			break
		}
		page.Items = append(page.Items, server)
	}
	return page, rows.Err()
}

func encodeServerCursor(cursor serverCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeServerCursor(value string) (serverCursor, error) {
	if value == "" {
		return serverCursor{}, nil
	}
	if len(value) > 256 {
		return serverCursor{}, errors.New("invalid server cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return serverCursor{}, errors.New("invalid server cursor")
	}
	var cursor serverCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.Name == "" || cursor.ID == "" {
		return serverCursor{}, errors.New("invalid server cursor")
	}
	return cursor, nil
}

type scanner interface{ Scan(...any) error }

func scanServer(row scanner) (contracts.Server, error) {
	var server contracts.Server
	var id, capabilitiesJSON, revision string
	var heartbeat sql.NullString
	if err := row.Scan(&id, &server.Name, &server.Address, &server.Role, &server.Architecture, &server.Platform, &capabilitiesJSON, &server.Version, &heartbeat, &server.ConnectionState, &server.FreshnessState, &server.FreshnessReason, &revision); err != nil {
		return contracts.Server{}, err
	}
	server.ID = contracts.ServerID(id)
	if err := json.Unmarshal([]byte(capabilitiesJSON), &server.Capabilities); err != nil {
		return contracts.Server{}, fmt.Errorf("decode server capabilities: %w", err)
	}
	if heartbeat.Valid && heartbeat.String != "" {
		parsed, err := time.Parse(time.RFC3339Nano, heartbeat.String)
		if err != nil {
			return contracts.Server{}, err
		}
		server.LastHeartbeat = &parsed
	}
	parsedRevision, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return contracts.Server{}, fmt.Errorf("decode server revision: %w", err)
	}
	server.ConfigurationRevision = parsedRevision
	return server, nil
}

func validateServer(server contracts.Server) error {
	if server.ID == "" || server.Name == "" || server.Role == "" || server.Architecture == "" || server.Platform == "" {
		return errors.New("server identity fields are required")
	}
	if len(server.ID) < 16 || len(server.ID) > 128 || !isSafeServerID(string(server.ID)) {
		return errors.New("server id must be a bounded URL-safe identifier")
	}
	if len(server.Name) > 128 || len(server.Address) > 255 || len(server.Role) > 32 || len(server.Architecture) > 32 || len(server.Platform) > 32 || len(server.Version) > 64 {
		return errors.New("server identity field exceeds limit")
	}
	if server.ConnectionState != "connected" && server.ConnectionState != "disconnected" && server.ConnectionState != "never-connected" && server.ConnectionState != "revoked" {
		return errors.New("invalid server connection state")
	}
	if server.FreshnessState != "fresh" && server.FreshnessState != "stale" && server.FreshnessState != "unknown" {
		return errors.New("invalid server freshness state")
	}
	if len(server.Capabilities) > 64 {
		return errors.New("server capabilities exceed limit")
	}
	for _, capability := range server.Capabilities {
		if len(capability) == 0 || len(capability) > 64 || !isSafeIdentifier(capability) {
			return errors.New("invalid server capability")
		}
	}
	return nil
}

func isSafeIdentifier(value string) bool {
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return value != ""
}

func isSafeMetricName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' && character != '.' && character != ':' {
			return false
		}
	}
	return true
}

func isSafeServerID(value string) bool {
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return value != ""
}

func (s *Store) IngestNodeBatch(ctx context.Context, serverID contracts.ServerID, samples []contracts.NodeMetricSample, gaps []contracts.CoverageGap, receivedAt time.Time) (IngestResult, error) {
	if receivedAt.IsZero() {
		return IngestResult{}, errors.New("received_at is required")
	}
	if serverID == "" {
		return IngestResult{}, errors.New("server_id is required")
	}
	converted := make([]contracts.MetricSample, 0, len(samples))
	for _, sample := range samples {
		if sample.ServerID != serverID {
			return IngestResult{}, errors.New("sample server_id does not match batch server_id")
		}
		stored, err := sample.WithReceivedAt(receivedAt)
		if err != nil {
			return IngestResult{}, err
		}
		converted = append(converted, stored)
	}
	return s.IngestSamples(ctx, serverID, converted, gaps)
}

func (s *Store) IngestSamples(ctx context.Context, serverID contracts.ServerID, samples []contracts.MetricSample, gaps []contracts.CoverageGap) (IngestResult, error) {
	if len(samples) > contracts.MaxBatchSamples || len(gaps) > contracts.MaxBatchSamples {
		return IngestResult{}, errors.New("sample batch exceeds limit")
	}
	if len(serverID) < 16 || len(serverID) > 128 || !isSafeServerID(string(serverID)) {
		return IngestResult{}, errors.New("server_id must be a bounded URL-safe identifier")
	}
	for _, sample := range samples {
		if err := sample.Validate(); err != nil {
			return IngestResult{}, err
		}
		if sample.ServerID != serverID {
			return IngestResult{}, errors.New("sample server_id does not match batch server_id")
		}
	}
	for _, gap := range gaps {
		if err := gap.Validate(); err != nil {
			return IngestResult{}, err
		}
	}
	if err := s.ensureWritable(ctx); err != nil {
		return IngestResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{}
	// Track the latest durable observation for each epoch touched by this
	// batch. Retention uses this metadata to retire inactive epochs, while raw
	// queue rows remain the stronger protection for delayed processing.
	epochLastSeen := make(map[contracts.CollectorEpoch]time.Time, len(samples)+len(gaps))
	for _, sample := range samples {
		if seen, ok := epochLastSeen[sample.CollectorEpoch]; !ok || sample.ReceivedAt.After(seen) {
			epochLastSeen[sample.CollectorEpoch] = sample.ReceivedAt
		}
	}
	gapSeenAt := time.Now().UTC()
	for _, gap := range gaps {
		if seen, ok := epochLastSeen[gap.CollectorEpoch]; !ok || gapSeenAt.After(seen) {
			epochLastSeen[gap.CollectorEpoch] = gapSeenAt
		}
	}
	for epoch, seenAt := range epochLastSeen {
		retired, unseenBlocked, retiredErr := collectorEpochAuthorityStatusTx(ctx, tx, serverID, epoch)
		if retiredErr != nil {
			_ = tx.Rollback()
			return IngestResult{}, retiredErr
		}
		if unseenBlocked {
			_ = tx.Rollback()
			return IngestResult{}, ErrCollectorEpochAuthorityFull
		}
		if retired {
			_ = tx.Rollback()
			return IngestResult{}, errors.New("collector epoch is retired")
		}
		if err := touchCollectorEpochTx(ctx, tx, serverID, epoch, seenAt); err != nil {
			_ = tx.Rollback()
			return IngestResult{}, err
		}
	}
	frontierStreams := make(map[string]contracts.CollectorEpoch)
	for _, sample := range samples {
		frontierStreams[string(sample.CollectorEpoch)+"\x00"+string(serverID)] = sample.CollectorEpoch
		inserted, err := insertSample(ctx, tx, sample)
		if err != nil {
			_ = tx.Rollback()
			return IngestResult{}, err
		}
		if inserted {
			var queued int
			if countErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM post_process_queue`).Scan(&queued); countErr != nil {
				_ = tx.Rollback()
				return IngestResult{}, countErr
			}
			if queued >= MaxPostProcessQueue {
				_ = tx.Rollback()
				return IngestResult{}, errors.New("post-process queue is full")
			}
			sampleJSON, marshalErr := json.Marshal(sample)
			if marshalErr != nil {
				_ = tx.Rollback()
				return IngestResult{}, marshalErr
			}
			if _, queueErr := tx.ExecContext(ctx, `INSERT INTO post_process_queue(server_id,collector_epoch,sequence,sample_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, string(serverID), string(sample.CollectorEpoch), strconv.FormatUint(sample.Sequence, 10), string(sampleJSON), FormatPersistedTime(time.Now())); queueErr != nil {
				_ = tx.Rollback()
				return IngestResult{}, queueErr
			}
			result.Inserted++
			result.InsertedSamples = append(result.InsertedSamples, sample)
		} else {
			result.Duplicate++
		}
	}
	for _, gap := range gaps {
		frontierStreams[string(gap.CollectorEpoch)+"\x00"+string(serverID)] = gap.CollectorEpoch
		fromObserved, toObserved, err := gapObservedBoundsTx(ctx, tx, serverID, gap)
		if err != nil {
			_ = tx.Rollback()
			return IngestResult{}, err
		}
		err = insertCoverageGapTx(ctx, tx, serverID, gap.CollectorEpoch, gap.FromSequence, gap.ToSequence, gap.Reason, fromObserved, toObserved)
		if err != nil {
			_ = tx.Rollback()
			return IngestResult{}, err
		}
	}
	for _, epoch := range frontierStreams {
		if err := advanceSequenceFrontierTx(ctx, tx, serverID, epoch); err != nil {
			_ = tx.Rollback()
			return IngestResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return IngestResult{}, err
	}
	s.observerMu.RLock()
	observer := s.observer
	s.observerMu.RUnlock()
	if observer != nil && (len(result.InsertedSamples) > 0 || len(gaps) > 0) {
		observerSamples := result.InsertedSamples
		if len(gaps) > 0 && observerSamples == nil {
			observerSamples = []contracts.MetricSample{}
		}
		if err := observer(ctx, observerSamples); err != nil {
			// Raw samples are already durable. Do not ask a node to resend them:
			// duplicates would not carry a newly inserted sample list. Surface the
			// bounded derived-processing failure to the caller for monitoring.
			result.PostProcessPending = true
		}
	}
	// Rollups are maintained as part of the durable ingestion path. This keeps
	// minute/hour API and CLI reads useful for normal transport/CLI ingestion,
	// not only for tests or an ad-hoc caller of PutRollups. Recompute the
	// affected bucket and its neighbors because a new point can change gauge
	// duration in the previous bucket and a counter interval in the next one.
	materializationSamples := append([]contracts.MetricSample(nil), samples...)
	if len(gaps) > 0 {
		boundarySamples, err := s.samplesForGaps(ctx, serverID, gaps)
		if err != nil {
			// Raw samples/gaps are already durable. Keep the acknowledgement
			// successful and queue all known affected buckets so a later
			// ingestion/query retries derived rollups even if no new sample arrives.
			_ = s.queueRollupRecovery(ctx, serverID, samples)
			result.RollupsPending = true
			return result, nil
		}
		materializationSamples = append(materializationSamples, boundarySamples...)
		if len(boundarySamples) == 0 {
			// A gap can outlive all of its boundary samples (for example after
			// retention). Queue existing rollup rows so their coverage is still
			// rebuilt even though there is no raw timestamp to derive a target.
			if err := s.queueRollupRecovery(ctx, serverID, nil); err != nil {
				result.RollupsPending = true
			}
		}
	}
	if len(materializationSamples) > 0 || len(gaps) > 0 {
		if err := s.materializeRollups(ctx, serverID, materializationSamples); err != nil {
			// Rollups are a recoverable derived view. Never turn a successful
			// durable sample commit into a failed acknowledgement because the
			// storage budget or a transient SQLite error blocked recomputation.
			result.RollupsPending = true
			return result, nil
		}
	}
	return result, nil
}

// HighestContiguousSequence returns the highest sequence in the durable
// sample set for one collector epoch, anchored at sequence zero. It returns
// found=false until sequence zero is durable; a later first arrival must not
// be treated as the beginning of the stream because doing so would let a node
// discard all of the unreceived samples before it. It intentionally stops at
// an unreported missing sequence; coverage gaps are records of loss and must
// not make a missing sample appear durably acknowledged.
func (s *Store) HighestContiguousSequence(ctx context.Context, serverID contracts.ServerID, epoch contracts.CollectorEpoch) (uint64, bool, error) {
	if serverID == "" || epoch == "" {
		return 0, false, errors.New("server and collector epoch are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence FROM metric_samples WHERE server_id=? AND collector_epoch=? ORDER BY length(sequence), sequence`, string(serverID), string(epoch))
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var through uint64
	expected := uint64(0)
	found := false
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return 0, false, err
		}
		sequence, err := strconv.ParseUint(encoded, 10, 64)
		if err != nil {
			return 0, false, err
		}
		if sequence < expected {
			continue
		}
		if sequence != expected {
			break
		}
		through, found = sequence, true
		if expected == ^uint64(0) {
			break
		}
		expected++
	}
	return through, found, rows.Err()
}

// HighestContiguousCoverageSequence returns the highest sequence covered by
// durable samples or explicit half-open coverage gaps, starting at zero. A
// reported gap is not a sample, but it is an authoritative record that the
// node intentionally dropped that range and may advance its spool frontier.
func (s *Store) HighestContiguousCoverageSequence(ctx context.Context, serverID contracts.ServerID, epoch contracts.CollectorEpoch) (uint64, bool, error) {
	if serverID == "" || epoch == "" {
		return 0, false, errors.New("server and collector epoch are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	frontier, maxPoint, err := ensureSequenceFrontierValueTx(ctx, tx, serverID, epoch)
	if err != nil {
		_ = tx.Rollback()
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	if frontier == 0 {
		return 0, false, nil
	}
	if frontier == ^uint64(0) && maxPoint {
		return ^uint64(0), true, nil
	}
	return frontier - 1, true, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// gapObservedBoundsTx resolves the nearest durable observations around a
// sequence gap. The bounds let time-range queries exclude unrelated historical
// gaps even after the raw sequence itself has been paginated.
func gapObservedBoundsTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, gap contracts.CoverageGap) (string, string, error) {
	from := strconv.FormatUint(gap.FromSequence, 10)
	to := strconv.FormatUint(gap.ToSequence, 10)
	var before, after sql.NullString
	beforeQuery := `SELECT observed_at FROM metric_samples WHERE server_id=? AND collector_epoch=? AND (length(sequence) < ? OR (length(sequence)=? AND sequence < ?)) ORDER BY length(sequence) DESC, sequence DESC LIMIT 1`
	if err := tx.QueryRowContext(ctx, beforeQuery, string(serverID), string(gap.CollectorEpoch), len(from), len(from), from).Scan(&before); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	afterQuery := `SELECT observed_at FROM metric_samples WHERE server_id=? AND collector_epoch=? AND (length(sequence) > ? OR (length(sequence)=? AND sequence >= ?)) ORDER BY length(sequence) ASC, sequence ASC LIMIT 1`
	if err := tx.QueryRowContext(ctx, afterQuery, string(serverID), string(gap.CollectorEpoch), len(to), len(to), to).Scan(&after); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	fromObserved, toObserved := "", ""
	if before.Valid {
		fromObserved = before.String
	}
	if after.Valid {
		toObserved = after.String
	}
	return fromObserved, toObserved, nil
}

func (s *Store) samplesForGaps(ctx context.Context, serverID contracts.ServerID, gaps []contracts.CoverageGap) ([]contracts.MetricSample, error) {
	const selectMetrics = `SELECT server_id,collector_epoch,sequence,observed_at,received_at,COALESCE(timestamp_uncertainty,''),values_json,COALESCE(counters_json,'{}'),COALESCE(units_json,'{}'),COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id=? AND collector_epoch=?`
	byIdentity := make(map[string]contracts.MetricSample)
	for _, gap := range gaps {
		from := strconv.FormatUint(gap.FromSequence, 10)
		to := strconv.FormatUint(gap.ToSequence, 10)
		queries := []struct {
			sql  string
			args []any
		}{
			{selectMetrics + ` AND (length(sequence) < ? OR (length(sequence) = ? AND sequence < ?)) ORDER BY length(sequence) DESC, sequence DESC LIMIT 1`, []any{string(serverID), string(gap.CollectorEpoch), len(from), len(from), from}},
			{selectMetrics + ` AND (length(sequence) > ? OR (length(sequence) = ? AND sequence >= ?)) ORDER BY length(sequence) ASC, sequence ASC LIMIT 1`, []any{string(serverID), string(gap.CollectorEpoch), len(to), len(to), to}},
		}
		for _, query := range queries {
			rows, err := s.db.QueryContext(ctx, query.sql, query.args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				sample, err := scanMetric(rows)
				if err != nil {
					_ = rows.Close()
					return nil, err
				}
				key := string(sample.CollectorEpoch) + "\x00" + strconv.FormatUint(sample.Sequence, 10)
				byIdentity[key] = sample
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	result := make([]contracts.MetricSample, 0, len(byIdentity))
	for _, sample := range byIdentity {
		result = append(result, sample)
	}
	return result, nil
}

type rollupTarget struct {
	start   time.Time
	seconds int
}

const maxRollupWindowSamples = 10000

func (s *Store) materializeRollups(ctx context.Context, serverID contracts.ServerID, samples []contracts.MetricSample) error {
	targets := make(map[rollupTarget]struct{}, len(samples)*6)
	queuedTargets, err := s.queuedRollupTargets(ctx, serverID)
	if err != nil {
		return err
	}
	for _, target := range queuedTargets {
		targets[target] = struct{}{}
	}
	for _, sample := range samples {
		for _, seconds := range []int{60, 3600} {
			bucket := sample.ObservedAt.UTC().Truncate(time.Duration(seconds) * time.Second)
			for _, delta := range []int{-1, 0, 1} {
				targets[rollupTarget{start: bucket.Add(time.Duration(delta*seconds) * time.Second), seconds: seconds}] = struct{}{}
			}
		}
		// A late sample can become the predecessor for a stored sample several
		// buckets later. Recompute the endpoint's bucket too; otherwise the old
		// predecessor delta remains there while the newly inserted interval is
		// added again in the late sample's neighborhood.
		nextSample, found, err := s.nextRollupSample(ctx, serverID, sample)
		if err != nil {
			return err
		}
		if found {
			for _, seconds := range []int{60, 3600} {
				targets[rollupTarget{start: nextSample.ObservedAt.UTC().Truncate(time.Duration(seconds) * time.Second), seconds: seconds}] = struct{}{}
			}
		}
	}
	orderedTargets := make([]rollupTarget, 0, len(targets))
	for target := range targets {
		orderedTargets = append(orderedTargets, target)
	}
	sort.Slice(orderedTargets, func(i, j int) bool {
		if orderedTargets[i].start.Equal(orderedTargets[j].start) {
			return orderedTargets[i].seconds < orderedTargets[j].seconds
		}
		return orderedTargets[i].start.Before(orderedTargets[j].start)
	})
	const maxTargetsPerMaterialize = 100
	if len(orderedTargets) > maxTargetsPerMaterialize {
		orderedTargets = orderedTargets[:maxTargetsPerMaterialize]
	}
	gaps, gapsTruncated, err := s.queryRollupGaps(ctx, serverID)
	if err != nil {
		_ = s.enqueueRollupTargets(ctx, serverID, orderedTargets)
		return err
	}

	for index, target := range orderedTargets {
		window, truncated, err := s.queryRollupSamples(ctx, serverID, target.start, target.seconds)
		if err != nil {
			_ = s.enqueueRollupTargets(ctx, serverID, orderedTargets[index:])
			return err
		}
		if len(window) == 0 {
			if err := s.removeQueuedRollupTarget(ctx, serverID, target); err != nil {
				return err
			}
			continue
		}
		rollups := BuildRollupsWithGaps(serverID, window, gaps, target.seconds)
		selected := make([]Rollup, 0, len(rollups))
		for _, rollup := range rollups {
			if !rollup.BucketStart.Equal(target.start) {
				continue
			}
			if truncated || gapsTruncated {
				rollup.Coverage = "uncertain"
				rollup.CounterDelta = ""
			}
			selected = append(selected, rollup)
		}
		for len(selected) > 0 {
			batchSize := MaxPageItems
			if len(selected) < batchSize {
				batchSize = len(selected)
			}
			if err := s.PutRollups(ctx, selected[:batchSize]); err != nil {
				_ = s.enqueueRollupTargets(ctx, serverID, orderedTargets[index:])
				return err
			}
			selected = selected[batchSize:]
		}
		if err := s.removeQueuedRollupTarget(ctx, serverID, target); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) queuedRollupTargets(ctx context.Context, serverID contracts.ServerID) ([]rollupTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT bucket_start,bucket_seconds FROM rollup_rebuild_queue WHERE server_id=? ORDER BY bucket_start,bucket_seconds`, string(serverID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]rollupTarget, 0)
	for rows.Next() {
		var start string
		var seconds int
		if err := rows.Scan(&start, &seconds); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, start)
		if err != nil || (seconds != 60 && seconds != 3600) {
			return nil, errors.New("invalid queued rollup target")
		}
		result = append(result, rollupTarget{start: parsed, seconds: seconds})
	}
	return result, rows.Err()
}

func (s *Store) enqueueRollupTargets(ctx context.Context, serverID contracts.ServerID, targets []rollupTarget) error {
	if len(targets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rollup_rebuild_queue(server_id,bucket_start,bucket_seconds) VALUES(?,?,?) ON CONFLICT DO NOTHING`, string(serverID), FormatPersistedTime(target.start), target.seconds); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) queueRollupRecovery(ctx context.Context, serverID contracts.ServerID, samples []contracts.MetricSample) error {
	targets := rollupTargetsForSamples(samples)
	for _, sample := range samples {
		next, found, err := s.nextRollupSample(ctx, serverID, sample)
		if err != nil {
			// Recovery queueing itself must remain best-effort when one malformed
			// boundary row is what caused the original lookup failure. Existing
			// persisted rollups are still queued below.
			continue
		}
		if !found {
			continue
		}
		for _, seconds := range []int{60, 3600} {
			targets = append(targets, rollupTarget{start: next.ObservedAt.UTC().Truncate(time.Duration(seconds) * time.Second), seconds: seconds})
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT bucket_start,bucket_seconds FROM metric_rollups WHERE server_id=?`, string(serverID))
	if err != nil {
		return err
	}
	for rows.Next() {
		var start string
		var seconds int
		if err := rows.Scan(&start, &seconds); err != nil {
			_ = rows.Close()
			return err
		}
		parsed, err := time.Parse(time.RFC3339Nano, start)
		if err != nil || (seconds != 60 && seconds != 3600) {
			_ = rows.Close()
			return errors.New("invalid persisted rollup target")
		}
		targets = append(targets, rollupTarget{start: parsed, seconds: seconds})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return s.enqueueRollupTargets(ctx, serverID, targets)
}

func rollupTargetsForSamples(samples []contracts.MetricSample) []rollupTarget {
	set := make(map[rollupTarget]struct{}, len(samples)*6)
	for _, sample := range samples {
		for _, seconds := range []int{60, 3600} {
			bucket := sample.ObservedAt.UTC().Truncate(time.Duration(seconds) * time.Second)
			for _, delta := range []int{-1, 0, 1} {
				set[rollupTarget{start: bucket.Add(time.Duration(delta*seconds) * time.Second), seconds: seconds}] = struct{}{}
			}
		}
	}
	targets := make([]rollupTarget, 0, len(set))
	for target := range set {
		targets = append(targets, target)
	}
	return targets
}

func (s *Store) removeQueuedRollupTarget(ctx context.Context, serverID contracts.ServerID, target rollupTarget) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rollup_rebuild_queue WHERE server_id=? AND bucket_start=? AND bucket_seconds=?`, string(serverID), FormatPersistedTime(target.start), target.seconds)
	return err
}

func (s *Store) nextRollupSample(ctx context.Context, serverID contracts.ServerID, sample contracts.MetricSample) (contracts.MetricSample, bool, error) {
	const selectMetrics = `SELECT server_id,collector_epoch,sequence,observed_at,received_at,COALESCE(timestamp_uncertainty,''),values_json,COALESCE(counters_json,'{}'),COALESCE(units_json,'{}'),COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id=?`
	sequence := strconv.FormatUint(sample.Sequence, 10)
	query := selectMetrics + ` AND (observed_at > ? OR (observed_at = ? AND (length(sequence) > ? OR (length(sequence) = ? AND (sequence > ? OR (sequence = ? AND collector_epoch > ?)))))) ORDER BY observed_at ASC, length(sequence) ASC, sequence ASC, collector_epoch ASC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, string(serverID), FormatPersistedTime(sample.ObservedAt), FormatPersistedTime(sample.ObservedAt), len(sequence), len(sequence), sequence, sequence, string(sample.CollectorEpoch))
	var server, epoch, observed, received, uncertainty, values, counters, units, validity string
	var sequenceValue string
	if err := row.Scan(&server, &epoch, &sequenceValue, &observed, &received, &uncertainty, &values, &counters, &units, &validity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contracts.MetricSample{}, false, nil
		}
		return contracts.MetricSample{}, false, err
	}
	parsedSequence, err := strconv.ParseUint(sequenceValue, 10, 64)
	if err != nil {
		return contracts.MetricSample{}, false, err
	}
	parsedObserved, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return contracts.MetricSample{}, false, err
	}
	parsedReceived, err := time.Parse(time.RFC3339Nano, received)
	if err != nil {
		return contracts.MetricSample{}, false, err
	}
	result := contracts.MetricSample{ServerID: contracts.ServerID(server), CollectorEpoch: contracts.CollectorEpoch(epoch), Sequence: parsedSequence, ObservedAt: parsedObserved, ReceivedAt: parsedReceived, TimestampUncertainty: uncertainty}
	if err := json.Unmarshal([]byte(values), &result.Values); err != nil {
		return contracts.MetricSample{}, false, err
	}
	if err := json.Unmarshal([]byte(counters), &result.Counters); err != nil {
		return contracts.MetricSample{}, false, err
	}
	if err := json.Unmarshal([]byte(units), &result.Units); err != nil {
		return contracts.MetricSample{}, false, err
	}
	if err := json.Unmarshal([]byte(validity), &result.Validity); err != nil {
		return contracts.MetricSample{}, false, err
	}
	return result, true, nil
}

func (s *Store) queryRollupSamples(ctx context.Context, serverID contracts.ServerID, bucketStart time.Time, bucketSeconds int) ([]contracts.MetricSample, bool, error) {
	if bucketSeconds != 60 && bucketSeconds != 3600 {
		return nil, false, errors.New("rollup bucket must be minute or hour")
	}
	const selectMetrics = `SELECT server_id,collector_epoch,sequence,observed_at,received_at,COALESCE(timestamp_uncertainty,''),values_json,COALESCE(counters_json,'{}'),COALESCE(units_json,'{}'),COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id=?`
	byIdentity := make(map[string]contracts.MetricSample)
	appendRows := func(rows *sql.Rows) error {
		defer rows.Close()
		for rows.Next() {
			sample, err := scanMetric(rows)
			if err != nil {
				return err
			}
			key := string(sample.CollectorEpoch) + "\x00" + strconv.FormatUint(sample.Sequence, 10)
			byIdentity[key] = sample
		}
		return rows.Err()
	}
	start := bucketStart.UTC()
	end := start.Add(time.Duration(bucketSeconds) * time.Second)
	rows, err := s.db.QueryContext(ctx, selectMetrics+` AND observed_at < ? ORDER BY observed_at DESC, length(sequence) DESC, sequence DESC LIMIT 1`, string(serverID), FormatPersistedTime(start))
	if err != nil {
		return nil, false, err
	}
	if err := appendRows(rows); err != nil {
		return nil, false, err
	}
	rows, err = s.db.QueryContext(ctx, selectMetrics+` AND observed_at >= ? AND observed_at < ? ORDER BY observed_at ASC, length(sequence) ASC, sequence ASC, collector_epoch ASC LIMIT ?`, string(serverID), FormatPersistedTime(start), FormatPersistedTime(end), maxRollupWindowSamples+1)
	if err != nil {
		return nil, false, err
	}
	if err := appendRows(rows); err != nil {
		return nil, false, err
	}
	truncated := len(byIdentity) > maxRollupWindowSamples
	rows, err = s.db.QueryContext(ctx, selectMetrics+` AND observed_at >= ? ORDER BY observed_at ASC, length(sequence) ASC, sequence ASC, collector_epoch ASC LIMIT 1`, string(serverID), FormatPersistedTime(end))
	if err != nil {
		return nil, false, err
	}
	if err := appendRows(rows); err != nil {
		return nil, false, err
	}
	ordered := make([]contracts.MetricSample, 0, len(byIdentity))
	for _, sample := range byIdentity {
		ordered = append(ordered, sample)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ObservedAt.Equal(ordered[j].ObservedAt) {
			if ordered[i].Sequence == ordered[j].Sequence {
				return ordered[i].CollectorEpoch < ordered[j].CollectorEpoch
			}
			return ordered[i].Sequence < ordered[j].Sequence
		}
		return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
	})
	return ordered, truncated, nil
}

func (s *Store) queryRollupGaps(ctx context.Context, serverID contracts.ServerID) ([]contracts.CoverageGap, bool, error) {
	const maxGaps = 10000
	rows, err := s.db.QueryContext(ctx, `SELECT collector_epoch,from_sequence,to_sequence,reason FROM coverage_gaps WHERE server_id=? ORDER BY collector_epoch, length(from_sequence), from_sequence LIMIT ?`, string(serverID), maxGaps+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	gaps := make([]contracts.CoverageGap, 0)
	truncated := false
	for rows.Next() {
		if len(gaps) == maxGaps {
			truncated = true
			break
		}
		var epoch, fromSequence, toSequence, reason string
		if err := rows.Scan(&epoch, &fromSequence, &toSequence, &reason); err != nil {
			return nil, false, err
		}
		from, err := strconv.ParseUint(fromSequence, 10, 64)
		if err != nil {
			return nil, false, err
		}
		to, err := strconv.ParseUint(toSequence, 10, 64)
		if err != nil {
			return nil, false, err
		}
		gaps = append(gaps, contracts.CoverageGap{CollectorEpoch: contracts.CollectorEpoch(epoch), FromSequence: from, ToSequence: to, Reason: reason})
	}
	return gaps, truncated, rows.Err()
}

func insertSample(ctx context.Context, tx *sql.Tx, sample contracts.MetricSample) (bool, error) {
	// A retention tombstone is an authoritative record that this sequence was
	// already accepted before its raw payload aged out. Treat a retransmission
	// as a duplicate across all future allowance configurations.
	tombstoned, err := metricSampleTombstonedTx(ctx, tx, sample.ServerID, sample.CollectorEpoch, sample.Sequence)
	if err != nil {
		return false, err
	}
	if tombstoned {
		return false, nil
	}
	values, err := json.Marshal(sample.Values)
	if err != nil {
		return false, err
	}
	counters, err := json.Marshal(sample.Counters)
	if err != nil {
		return false, err
	}
	units, err := json.Marshal(sample.Units)
	if err != nil {
		return false, err
	}
	validity, err := json.Marshal(sample.Validity)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO metric_samples(server_id,collector_epoch,sequence,observed_at,received_at,timestamp_uncertainty,values_json,counters_json,units_json,validity_json) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		string(sample.ServerID), string(sample.CollectorEpoch), strconv.FormatUint(sample.Sequence, 10), FormatPersistedTime(sample.ObservedAt), FormatPersistedTime(sample.ReceivedAt), sample.TimestampUncertainty, string(values), string(counters), string(units), string(validity))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

type metricCursor struct {
	ObservedAt     string `json:"observed_at"`
	Sequence       string `json:"sequence"`
	CollectorEpoch string `json:"collector_epoch"`
}

type gapCursor struct {
	CollectorEpoch string `json:"collector_epoch"`
	FromSequence   string `json:"from_sequence"`
	ToSequence     string `json:"to_sequence"`
	Reason         string `json:"reason"`
}

func encodeCursor(cursor metricCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(value string) (metricCursor, error) {
	if value == "" {
		return metricCursor{}, nil
	}
	if len(value) > 256 {
		return metricCursor{}, errors.New("invalid cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return metricCursor{}, errors.New("invalid cursor")
	}
	var cursor metricCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.ObservedAt == "" || cursor.Sequence == "" || cursor.CollectorEpoch == "" {
		return metricCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}

func (s *Store) QueryMetrics(ctx context.Context, serverID contracts.ServerID, from, to time.Time, limit int, cursor string) (MetricPage, error) {
	return s.QueryMetricsWithGapCursor(ctx, serverID, from, to, limit, cursor, "")
}

// QueryMetricSamplesRange returns the newest points in a bounded raw sample
// window for internal
// consumers that need to perform timestamp-aware calculations (for example a
// recent-rate traffic forecast). Unlike QueryMetrics it does not construct a
// response page or materialize rollups, and it reports truncation explicitly
// so a caller never mistakes an incomplete window for full coverage.
func (s *Store) QueryMetricSamplesRange(ctx context.Context, serverID contracts.ServerID, from, to time.Time, limit int) ([]contracts.MetricSample, bool, error) {
	if !validStoreServerID(serverID) {
		return nil, false, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if limit < 1 || limit > MaxForecastSamples {
		return nil, false, errors.New("metric forecast sample limit is outside the permitted bound")
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return nil, false, errors.New("metric sample range is invalid")
	}
	query := `SELECT server_id,collector_epoch,sequence,observed_at,received_at,COALESCE(timestamp_uncertainty,''),values_json,COALESCE(counters_json,'{}'),COALESCE(units_json,'{}'),COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id=?`
	args := []any{string(serverID)}
	if !from.IsZero() {
		query += ` AND observed_at >= ?`
		args = append(args, FormatPersistedTime(from))
	}
	if !to.IsZero() {
		query += ` AND observed_at <= ?`
		args = append(args, FormatPersistedTime(to))
	}
	// Keep the tail when a high-cadence window exceeds the resource bound. A
	// recent-rate consumer can still make a sound decision from a complete
	// recent tail; returning the oldest rows would omit the as-of edge and make
	// every fallback stale. Reverse below to preserve chronological output.
	query += ` ORDER BY observed_at DESC, length(sequence) DESC, sequence DESC, collector_epoch DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	capacity := limit
	if capacity > MaxPageItems*4 {
		capacity = MaxPageItems * 4
	}
	samples := make([]contracts.MetricSample, 0, capacity)
	truncated := false
	for rows.Next() {
		if len(samples) == limit {
			truncated = true
			break
		}
		sample, scanErr := scanMetric(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	for left, right := 0, len(samples)-1; left < right; left, right = left+1, right-1 {
		samples[left], samples[right] = samples[right], samples[left]
	}
	return samples, truncated, nil
}

func (s *Store) QueryMetricsWithGapCursor(ctx context.Context, serverID contracts.ServerID, from, to time.Time, limit int, cursor, gapsCursor string) (MetricPage, error) {
	limit = clampLimit(limit)
	decoded, err := decodeCursor(cursor)
	if err != nil {
		return MetricPage{}, err
	}
	decodedGap, err := decodeGapCursor(gapsCursor)
	if err != nil {
		return MetricPage{}, err
	}
	query := `SELECT server_id,collector_epoch,sequence,observed_at,received_at,COALESCE(timestamp_uncertainty,''),values_json,COALESCE(counters_json,'{}'),COALESCE(units_json,'{}'),COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id=?`
	args := []any{string(serverID)}
	if !from.IsZero() {
		query += ` AND observed_at >= ?`
		args = append(args, FormatPersistedTime(from))
	}
	if !to.IsZero() {
		query += ` AND observed_at <= ?`
		args = append(args, FormatPersistedTime(to))
	}
	if decoded.ObservedAt != "" {
		if decoded.CollectorEpoch == "" {
			return MetricPage{}, errors.New("invalid metric cursor")
		}
		query += ` AND (observed_at > ? OR (observed_at = ? AND (length(sequence) > ? OR (length(sequence) = ? AND (sequence > ? OR (sequence = ? AND collector_epoch > ?))))) )`
		args = append(args, decoded.ObservedAt, decoded.ObservedAt, len(decoded.Sequence), len(decoded.Sequence), decoded.Sequence, decoded.Sequence, decoded.CollectorEpoch)
	}
	query += ` ORDER BY observed_at ASC, length(sequence) ASC, sequence ASC, collector_epoch ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return MetricPage{}, err
	}
	page := MetricPage{Samples: make([]contracts.MetricSample, 0, limit), Rollups: make([]Rollup, 0), Coverage: map[string]float64{}}
	responseBytes := 0
	const metricResponseHeadroom = 128 << 10 // coverage, gaps, and JSON envelope
	for rows.Next() {
		sample, err := scanMetric(rows)
		if err != nil {
			return MetricPage{}, err
		}
		if len(page.Samples) == limit {
			page.Truncated = true
			last := page.Samples[len(page.Samples)-1]
			page.NextCursor = encodeCursor(metricCursor{ObservedAt: FormatPersistedTime(last.ObservedAt), Sequence: strconv.FormatUint(last.Sequence, 10), CollectorEpoch: string(last.CollectorEpoch)})
			break
		}
		encodedSample, marshalErr := json.Marshal(sample)
		if marshalErr != nil {
			return MetricPage{}, marshalErr
		}
		if responseBytes+len(encodedSample)+2 > contracts.MaxEnvelopeBytes-metricResponseHeadroom {
			if len(page.Samples) == 0 {
				return MetricPage{}, errors.New("metric sample exceeds response size bound")
			}
			page.Truncated = true
			last := page.Samples[len(page.Samples)-1]
			page.NextCursor = encodeCursor(metricCursor{ObservedAt: FormatPersistedTime(last.ObservedAt), Sequence: strconv.FormatUint(last.Sequence, 10), CollectorEpoch: string(last.CollectorEpoch)})
			break
		}
		page.Samples = append(page.Samples, sample)
		responseBytes += len(encodedSample) + 1
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return MetricPage{}, err
	}
	if err := rows.Close(); err != nil {
		return MetricPage{}, err
	}
	gapQuery := `SELECT g.collector_epoch,g.from_sequence,g.to_sequence,g.reason,COALESCE(g.from_observed_at,''),COALESCE(g.to_observed_at,'') FROM coverage_gaps g WHERE g.server_id=?`
	gapArgs := []any{string(serverID)}
	if !from.IsZero() {
		gapQuery += ` AND (g.to_observed_at IS NULL OR g.to_observed_at >= ?)`
		gapArgs = append(gapArgs, FormatPersistedTime(from))
	}
	if !to.IsZero() {
		gapQuery += ` AND (g.from_observed_at IS NULL OR g.from_observed_at <= ?)`
		gapArgs = append(gapArgs, FormatPersistedTime(to))
	}
	if decodedGap.CollectorEpoch != "" {
		gapQuery += ` AND (collector_epoch > ? OR (collector_epoch = ? AND (length(from_sequence) > ? OR (length(from_sequence) = ? AND (from_sequence > ? OR (from_sequence = ? AND (to_sequence > ? OR (to_sequence = ? AND reason > ?))))))) )`
		gapArgs = append(gapArgs, decodedGap.CollectorEpoch, decodedGap.CollectorEpoch, len(decodedGap.FromSequence), len(decodedGap.FromSequence), decodedGap.FromSequence, decodedGap.FromSequence, decodedGap.ToSequence, decodedGap.ToSequence, decodedGap.Reason)
	}
	gapQuery += ` ORDER BY collector_epoch, length(from_sequence), from_sequence, to_sequence, reason LIMIT ?`
	gapArgs = append(gapArgs, limit+1)
	gapRows, err := s.db.QueryContext(ctx, gapQuery, gapArgs...)
	if err != nil {
		return MetricPage{}, err
	}
	defer gapRows.Close()
	for gapRows.Next() {
		if len(page.Gaps) == limit {
			page.Truncated = true
			last := page.Gaps[len(page.Gaps)-1]
			page.GapsNextCursor = encodeGapCursor(gapCursor{CollectorEpoch: string(last.CollectorEpoch), FromSequence: strconv.FormatUint(last.FromSequence, 10), ToSequence: strconv.FormatUint(last.ToSequence, 10), Reason: last.Reason})
			break
		}
		var epoch, fromSequence, toSequence, reason, fromObserved, toObserved string
		if err := gapRows.Scan(&epoch, &fromSequence, &toSequence, &reason, &fromObserved, &toObserved); err != nil {
			return MetricPage{}, err
		}
		fromValue, err := strconv.ParseUint(fromSequence, 10, 64)
		if err != nil {
			return MetricPage{}, err
		}
		toValue, err := strconv.ParseUint(toSequence, 10, 64)
		if err != nil {
			return MetricPage{}, err
		}
		page.Gaps = append(page.Gaps, contracts.CoverageGap{CollectorEpoch: contracts.CollectorEpoch(epoch), FromSequence: fromValue, ToSequence: toValue, Reason: reason})
	}
	return page, gapRows.Err()
}

// PreviousMetricSample returns the immediately preceding durable sample in
// one collector epoch. Sequence ordering is numeric even though SQLite stores
// decimal strings to preserve uint64 values exactly.
func (s *Store) PreviousMetricSample(ctx context.Context, serverID contracts.ServerID, epoch contracts.CollectorEpoch, sequence uint64) (contracts.MetricSample, bool, error) {
	if !validStoreServerID(serverID) || epoch == "" || len(epoch) > 128 {
		return contracts.MetricSample{}, false, errors.New("invalid metric sample identity")
	}
	sequenceText := strconv.FormatUint(sequence, 10)
	row := s.db.QueryRowContext(ctx, `SELECT server_id,collector_epoch,sequence,observed_at,received_at,COALESCE(timestamp_uncertainty,''),values_json,COALESCE(counters_json,'{}'),COALESCE(units_json,'{}'),COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id=? AND collector_epoch=? AND (length(sequence) < ? OR (length(sequence)=? AND sequence < ?)) ORDER BY length(sequence) DESC, sequence DESC LIMIT 1`, string(serverID), string(epoch), len(sequenceText), len(sequenceText), sequenceText)
	sample, err := scanMetric(row)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.MetricSample{}, false, nil
	}
	return sample, err == nil, err
}

func encodeGapCursor(cursor gapCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeGapCursor(value string) (gapCursor, error) {
	if value == "" {
		return gapCursor{}, nil
	}
	if len(value) > 256 {
		return gapCursor{}, errors.New("invalid coverage gap cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return gapCursor{}, errors.New("invalid coverage gap cursor")
	}
	var cursor gapCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.CollectorEpoch == "" || cursor.FromSequence == "" || cursor.ToSequence == "" || cursor.Reason == "" || !decimalCounterString(cursor.FromSequence) || !decimalCounterString(cursor.ToSequence) {
		return gapCursor{}, errors.New("invalid coverage gap cursor")
	}
	return cursor, nil
}

type trafficCursor struct {
	PeriodStart string `json:"period_start"`
	Scope       string `json:"scope"`
	Direction   string `json:"direction"`
}

// UpsertTrafficPeriod stores a calendar period chosen by the caller. The
// store does not derive month boundaries; it preserves the supplied timezone
// and continuity so package 05 can own calendar policy without losing data.
func (s *Store) UpsertTrafficPeriod(ctx context.Context, serverID contracts.ServerID, period contracts.TrafficPeriod) error {
	if !validStoreServerID(serverID) {
		return errors.New("server_id must be a bounded URL-safe identifier")
	}
	if err := period.Validate(); err != nil {
		return err
	}
	protected, err := s.isProtectedTrafficPeriod(ctx, serverID, period)
	if err != nil {
		return err
	}
	if !protected {
		if err := s.ensureWritable(ctx); err != nil {
			return err
		}
	}
	interfaces, err := json.Marshal(period.Interfaces)
	if err != nil {
		return err
	}
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		from := FormatPersistedTime(period.From)
		to := FormatPersistedTime(period.To)
		var overlaps int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traffic_periods WHERE server_id=? AND scope=? AND direction=? AND period_start<>? AND period_start < ? AND period_end > ?)`, string(serverID), period.Scope, period.Direction, from, to, from).Scan(&overlaps); err != nil {
			return err
		}
		if overlaps == 1 {
			return errors.New("traffic period overlaps an existing period")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO traffic_periods(server_id,scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json) VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(server_id,scope,period_start,direction) DO UPDATE SET period_end=excluded.period_end,timezone=excluded.timezone,allowance_bytes=excluded.allowance_bytes,counted_bytes=excluded.counted_bytes,continuity=excluded.continuity,interfaces_json=excluded.interfaces_json`,
			string(serverID), period.Scope, from, to, period.Timezone, strconv.FormatUint(period.AllowanceBytes, 10), period.Direction, strconv.FormatUint(period.CountedBytes, 10), period.Continuity, string(interfaces))
		return err
	})
}

func (s *Store) isProtectedTrafficPeriod(ctx context.Context, serverID contracts.ServerID, period contracts.TrafficPeriod) (bool, error) {
	// Current billing state is protected even when ordinary history writes are
	// paused. Existing rows are also updates to protected state: rejecting one
	// under pressure would leave a monthly total stale at the exact point where
	// history retention is already degraded.
	now := time.Now().UTC()
	if !now.Before(period.From.UTC()) && now.Before(period.To.UTC()) {
		return true, nil
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traffic_periods WHERE server_id=? AND scope=? AND period_start=? AND direction=?)`, string(serverID), period.Scope, FormatPersistedTime(period.From), period.Direction).Scan(&exists)
	return exists == 1, err
}

func (s *Store) QueryTrafficPeriods(ctx context.Context, serverID contracts.ServerID, from, to time.Time, scope string, limit int, cursor string) (TrafficPage, error) {
	return s.queryTrafficPeriods(ctx, serverID, from, to, scope, "", limit, cursor)
}

// QueryTrafficPeriodsByDirection is the bounded, identity-scoped form used
// by allowance consumers. A scope may contain independent inbound, outbound,
// and combined periods; filtering direction in SQL prevents one allowance's
// historical interface snapshot from being selected for another forecast.
func (s *Store) QueryTrafficPeriodsByDirection(ctx context.Context, serverID contracts.ServerID, from, to time.Time, scope, direction string, limit int, cursor string) (TrafficPage, error) {
	if direction != "inbound" && direction != "outbound" && direction != "combined" {
		return TrafficPage{}, errors.New("traffic query direction is invalid")
	}
	return s.queryTrafficPeriods(ctx, serverID, from, to, scope, direction, limit, cursor)
}

func (s *Store) queryTrafficPeriods(ctx context.Context, serverID contracts.ServerID, from, to time.Time, scope, direction string, limit int, cursor string) (TrafficPage, error) {
	if !validStoreServerID(serverID) {
		return TrafficPage{}, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if !from.IsZero() && !to.IsZero() {
		if !to.After(from) {
			return TrafficPage{}, errors.New("traffic query end must be after start")
		}
		if to.Sub(from) > 31*24*time.Hour {
			return TrafficPage{}, errors.New("traffic query range exceeds 31 days")
		}
	}
	limit = clampLimit(limit)
	decoded, err := decodeTrafficCursor(cursor)
	if err != nil {
		return TrafficPage{}, err
	}
	query := `SELECT scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json FROM traffic_periods WHERE server_id=?`
	args := []any{string(serverID)}
	if !from.IsZero() {
		query += ` AND period_end > ?`
		args = append(args, FormatPersistedTime(from))
	}
	if !to.IsZero() {
		query += ` AND period_start < ?`
		args = append(args, FormatPersistedTime(to))
	}
	if scope != "" {
		if len(scope) > 128 || !isSafeMetricName(scope) {
			return TrafficPage{}, errors.New("scope is invalid")
		}
		query += ` AND scope=?`
		args = append(args, scope)
	}
	if direction != "" {
		query += ` AND direction=?`
		args = append(args, direction)
	}
	if decoded.PeriodStart != "" {
		query += ` AND (period_start > ? OR (period_start = ? AND (scope > ? OR (scope = ? AND direction > ?))) )`
		args = append(args, decoded.PeriodStart, decoded.PeriodStart, decoded.Scope, decoded.Scope, decoded.Direction)
	}
	query += ` ORDER BY period_start ASC, scope ASC, direction ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return TrafficPage{}, err
	}
	defer rows.Close()
	page := TrafficPage{Periods: make([]contracts.TrafficPeriod, 0, limit)}
	for rows.Next() {
		var period contracts.TrafficPeriod
		var periodStart, periodEnd, allowance, counted, interfaces string
		if err := rows.Scan(&period.Scope, &periodStart, &periodEnd, &period.Timezone, &allowance, &period.Direction, &counted, &period.Continuity, &interfaces); err != nil {
			return TrafficPage{}, err
		}
		if err := json.Unmarshal([]byte(interfaces), &period.Interfaces); err != nil {
			return TrafficPage{}, err
		}
		period.From, err = time.Parse(time.RFC3339Nano, periodStart)
		if err != nil {
			return TrafficPage{}, err
		}
		period.To, err = time.Parse(time.RFC3339Nano, periodEnd)
		if err != nil {
			return TrafficPage{}, err
		}
		period.AllowanceBytes, err = strconv.ParseUint(allowance, 10, 64)
		if err != nil {
			return TrafficPage{}, err
		}
		period.CountedBytes, err = strconv.ParseUint(counted, 10, 64)
		if err != nil {
			return TrafficPage{}, err
		}
		if err := period.Validate(); err != nil {
			return TrafficPage{}, err
		}
		if len(page.Periods) == limit {
			page.Truncated = true
			last := page.Periods[len(page.Periods)-1]
			page.NextCursor = encodeTrafficCursor(trafficCursor{PeriodStart: FormatPersistedTime(last.From), Scope: last.Scope, Direction: last.Direction})
			break
		}
		page.Periods = append(page.Periods, period)
	}
	return page, rows.Err()
}

// GetActiveTrafficPeriod returns the durable period containing at. Ordering by
// latest start remains defensive for databases created before explicit period
// handoff became non-overlapping.
func (s *Store) GetActiveTrafficPeriod(ctx context.Context, serverID contracts.ServerID, scope, direction string, at time.Time) (contracts.TrafficPeriod, bool, error) {
	if !validStoreServerID(serverID) || scope == "" || len(scope) > 128 || !isSafeMetricName(scope) || (direction != "inbound" && direction != "outbound" && direction != "combined") || at.IsZero() {
		return contracts.TrafficPeriod{}, false, errors.New("invalid traffic period identity")
	}
	row := s.db.QueryRowContext(ctx, `SELECT scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json FROM traffic_periods WHERE server_id=? AND scope=? AND direction=? AND period_start <= ? AND period_end > ? ORDER BY period_start DESC LIMIT 1`, string(serverID), scope, direction, FormatPersistedTime(at), FormatPersistedTime(at))
	var period contracts.TrafficPeriod
	var from, to, allowance, counted, interfaces string
	if err := row.Scan(&period.Scope, &from, &to, &period.Timezone, &allowance, &period.Direction, &counted, &period.Continuity, &interfaces); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contracts.TrafficPeriod{}, false, nil
		}
		return contracts.TrafficPeriod{}, false, err
	}
	var err error
	period.From, err = time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	period.To, err = time.Parse(time.RFC3339Nano, to)
	if err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	period.AllowanceBytes, err = strconv.ParseUint(allowance, 10, 64)
	if err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	period.CountedBytes, err = strconv.ParseUint(counted, 10, 64)
	if err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	if err := json.Unmarshal([]byte(interfaces), &period.Interfaces); err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	if err := period.Validate(); err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	return period, true, nil
}

func encodeTrafficCursor(cursor trafficCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeTrafficCursor(value string) (trafficCursor, error) {
	if value == "" {
		return trafficCursor{}, nil
	}
	if len(value) > 256 {
		return trafficCursor{}, errors.New("invalid traffic cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return trafficCursor{}, errors.New("invalid traffic cursor")
	}
	var cursor trafficCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.PeriodStart == "" || cursor.Scope == "" || cursor.Direction == "" {
		return trafficCursor{}, errors.New("invalid traffic cursor")
	}
	return cursor, nil
}

func scanMetric(row scanner) (contracts.MetricSample, error) {
	var sample contracts.MetricSample
	var serverID, epoch, sequence, observed, received, values, counters, units, validity, uncertainty string
	if err := row.Scan(&serverID, &epoch, &sequence, &observed, &received, &uncertainty, &values, &counters, &units, &validity); err != nil {
		return contracts.MetricSample{}, err
	}
	var err error
	sample.ServerID, sample.CollectorEpoch = contracts.ServerID(serverID), contracts.CollectorEpoch(epoch)
	sample.Sequence, err = strconv.ParseUint(sequence, 10, 64)
	if err != nil {
		return contracts.MetricSample{}, err
	}
	sample.ObservedAt, err = time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return contracts.MetricSample{}, err
	}
	sample.ReceivedAt, err = time.Parse(time.RFC3339Nano, received)
	if err != nil {
		return contracts.MetricSample{}, err
	}
	sample.TimestampUncertainty = uncertainty
	if err := json.Unmarshal([]byte(values), &sample.Values); err != nil {
		return contracts.MetricSample{}, err
	}
	if err := json.Unmarshal([]byte(counters), &sample.Counters); err != nil {
		return contracts.MetricSample{}, err
	}
	if err := json.Unmarshal([]byte(units), &sample.Units); err != nil {
		return contracts.MetricSample{}, err
	}
	if err := json.Unmarshal([]byte(validity), &sample.Validity); err != nil {
		return contracts.MetricSample{}, err
	}
	return sample, nil
}

func (s *Store) RegisterLogSource(ctx context.Context, source LogSource) error {
	if source.ServerID == "" || source.ID == "" || len(source.ID) > 128 || source.Label == "" || len(source.Label) > 256 {
		return errors.New("log source identity and label are required")
	}
	if !isSafeIdentifier(source.ID) {
		return errors.New("log source id must be a bounded identifier")
	}
	if source.Path != "" {
		if !filepath.IsAbs(source.Path) {
			return errors.New("log source path must be absolute")
		}
		if err := validateConfiguredPath(filepath.Clean(source.Path)); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO log_sources(server_id,id,label,path) VALUES(?,?,?,?) ON CONFLICT(server_id,id) DO UPDATE SET label=excluded.label,path=excluded.path`, string(source.ServerID), source.ID, source.Label, source.Path)
	return err
}

func (s *Store) ListLogSources(ctx context.Context, serverID contracts.ServerID, limit int) ([]LogSource, error) {
	page, err := s.QueryLogSourcePage(ctx, serverID, limit, "")
	return page.Items, err
}

// GetLogSource performs an exact lookup for authorization-sensitive live-tail
// requests. It is intentionally not implemented as a first-page list: a
// server may have more than MaxPageItems registered sources.
func (s *Store) GetLogSource(ctx context.Context, serverID contracts.ServerID, sourceID string) (LogSource, bool, error) {
	if serverID == "" || sourceID == "" || len(sourceID) > 128 || !isSafeIdentifier(sourceID) {
		return LogSource{}, false, errors.New("invalid log source identity")
	}
	var source LogSource
	source.ServerID = serverID
	err := s.db.QueryRowContext(ctx, `SELECT id,label,path FROM log_sources WHERE server_id=? AND id=?`, string(serverID), sourceID).Scan(&source.ID, &source.Label, &source.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return LogSource{}, false, nil
	}
	if err != nil {
		return LogSource{}, false, err
	}
	return source, true, nil
}

func (s *Store) QueryLogSourcePage(ctx context.Context, serverID contracts.ServerID, limit int, cursor string) (LogSourcePage, error) {
	if serverID == "" {
		return LogSourcePage{}, errors.New("server_id is required")
	}
	limit = clampLimit(limit)
	decoded, err := decodeLogSourceCursor(cursor)
	if err != nil {
		return LogSourcePage{}, err
	}
	query := `SELECT id,label,path FROM log_sources WHERE server_id=?`
	args := []any{string(serverID)}
	if decoded != "" {
		query += ` AND id > ?`
		args = append(args, decoded)
	}
	query += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return LogSourcePage{}, err
	}
	defer rows.Close()
	page := LogSourcePage{Items: make([]LogSource, 0, limit)}
	for rows.Next() {
		var source LogSource
		source.ServerID = serverID
		if err := rows.Scan(&source.ID, &source.Label, &source.Path); err != nil {
			return LogSourcePage{}, err
		}
		if len(page.Items) == limit {
			page.Truncated = true
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, source)
	}
	return page, rows.Err()
}

func decodeLogSourceCursor(value string) (string, error) {
	if value == "" || (len(value) <= 128 && isSafeIdentifier(value)) {
		return value, nil
	}
	return "", errors.New("invalid log source cursor")
}

func (s *Store) InsertLogEntries(ctx context.Context, entries []LogEntry) (int, error) {
	if len(entries) > MaxPageItems {
		return 0, errors.New("log page exceeds limit")
	}
	if err := s.ensureWritable(ctx); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, entry := range entries {
		if len(entry.ServerID) < 16 || len(entry.ServerID) > 128 || !isSafeServerID(string(entry.ServerID)) || entry.SourceID == "" || len(entry.SourceID) > 128 || entry.Cursor == "" || len(entry.Cursor) > 256 || entry.Timestamp.IsZero() || len(entry.Severity) > 32 || len(entry.Text) > 65536 || !utf8.ValidString(entry.Text) {
			_ = tx.Rollback()
			return 0, errors.New("invalid log entry")
		}
		// Redaction is enforced again at the persistence boundary. Adapters are
		// expected to redact, but transport or journal callers must not be able
		// to bypass the guarantee by setting Redacted themselves.
		redactedText, detected := redact(entry.Text)
		redacted := entry.Redacted || detected
		result, err := tx.ExecContext(ctx, `INSERT INTO log_entries(server_id,source_id,cursor,timestamp,severity,text,truncated,redacted) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, string(entry.ServerID), entry.SourceID, entry.Cursor, FormatPersistedTime(entry.Timestamp), entry.Severity, redactedText, boolInt(entry.Truncated), boolInt(redacted))
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		inserted += int(count)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

type logCursor struct {
	Timestamp string `json:"timestamp"`
	Cursor    string `json:"cursor"`
}

func (s *Store) QueryLogs(ctx context.Context, serverID contracts.ServerID, sourceID string, from, to time.Time, limit int, cursor string) (LogPage, error) {
	return s.QueryLogsFiltered(ctx, serverID, sourceID, from, to, "", "", limit, cursor)
}

// QueryLogsFiltered applies the bounded server-side severity and text filters
// required by the local log API. QueryLogs remains as a source-compatible
// wrapper for callers that do not need filtering.
func (s *Store) QueryLogsFiltered(ctx context.Context, serverID contracts.ServerID, sourceID string, from, to time.Time, severity, search string, limit int, cursor string) (LogPage, error) {
	if serverID == "" || sourceID == "" || len(sourceID) > 128 || !isSafeIdentifier(sourceID) {
		return LogPage{}, errors.New("log source identity is invalid")
	}
	if len(severity) > 32 || len(search) > 256 {
		return LogPage{}, errors.New("log filter exceeds limit")
	}
	limit = clampLimit(limit)
	decoded, err := decodeLogCursor(cursor)
	if err != nil {
		return LogPage{}, err
	}
	query := `SELECT source_id,cursor,timestamp,COALESCE(severity,''),text,truncated,redacted FROM log_entries WHERE server_id=? AND source_id=?`
	args := []any{string(serverID), sourceID}
	if !from.IsZero() {
		query += ` AND timestamp >= ?`
		args = append(args, FormatPersistedTime(from))
	}
	if !to.IsZero() {
		query += ` AND timestamp <= ?`
		args = append(args, FormatPersistedTime(to))
	}
	if severity != "" {
		query += ` AND severity = ?`
		args = append(args, severity)
	}
	if search != "" {
		query += ` AND instr(lower(text), lower(?)) > 0`
		args = append(args, search)
	}
	if decoded.Timestamp != "" {
		cursorTime, err := time.Parse(time.RFC3339Nano, decoded.Timestamp)
		if err != nil {
			return LogPage{}, errors.New("invalid log cursor")
		}
		persistedCursor := FormatPersistedTime(cursorTime)
		query += ` AND (timestamp > ? OR (timestamp = ? AND cursor > ?))`
		args = append(args, persistedCursor, persistedCursor, decoded.Cursor)
	}
	query += ` ORDER BY timestamp ASC, cursor ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return LogPage{}, err
	}
	defer rows.Close()
	page := LogPage{Entries: make([]LogEntry, 0, limit)}
	responseBytes := 0
	const responseHeadroom = 1024 // envelope fields, JSON array punctuation, and error-safe margin
	for rows.Next() {
		var entry LogEntry
		var timestamp string
		var truncated, redacted int
		if err := rows.Scan(&entry.SourceID, &entry.Cursor, &timestamp, &entry.Severity, &entry.Text, &truncated, &redacted); err != nil {
			return LogPage{}, err
		}
		entry.ServerID = serverID
		entry.Timestamp, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return LogPage{}, err
		}
		entry.Truncated, entry.Redacted = truncated != 0, redacted != 0
		if len(page.Entries) == limit {
			page.Truncated = true
			last := page.Entries[len(page.Entries)-1]
			page.NextCursor = encodeLogCursor(logCursor{Timestamp: FormatPersistedTime(last.Timestamp), Cursor: last.Cursor})
			break
		}
		encodedEntry, marshalErr := json.Marshal(entry)
		if marshalErr != nil {
			return LogPage{}, marshalErr
		}
		if len(page.Entries) > 0 && responseBytes+len(encodedEntry)+2 > contracts.MaxEnvelopeBytes-responseHeadroom {
			page.Truncated = true
			last := page.Entries[len(page.Entries)-1]
			page.NextCursor = encodeLogCursor(logCursor{Timestamp: FormatPersistedTime(last.Timestamp), Cursor: last.Cursor})
			break
		}
		page.Entries = append(page.Entries, entry)
		responseBytes += len(encodedEntry) + 1
	}
	return page, rows.Err()
}

func encodeLogCursor(cursor logCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeLogCursor(value string) (logCursor, error) {
	if value == "" {
		return logCursor{}, nil
	}
	if len(value) > 256 {
		return logCursor{}, errors.New("invalid log cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return logCursor{}, errors.New("invalid log cursor")
	}
	var cursor logCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.Timestamp == "" || cursor.Cursor == "" {
		return logCursor{}, errors.New("invalid log cursor")
	}
	return cursor, nil
}

// DecodeLogCursor exposes only the source cursor component to the CLI/source
// adapters; pagination ordering remains owned by QueryLogsFiltered.
func DecodeLogCursor(value string) (string, error) {
	cursor, err := decodeLogCursor(value)
	if err != nil {
		return "", err
	}
	return cursor.Cursor, nil
}

func (s *Store) PutRollups(ctx context.Context, rollups []Rollup) error {
	if len(rollups) > MaxPageItems {
		return errors.New("rollup page exceeds limit")
	}
	if err := s.ensureDerivedWritable(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, rollup := range rollups {
		if rollup.ObservedSeconds > float64(rollup.BucketSeconds) {
			rollup.ObservedSeconds = float64(rollup.BucketSeconds)
		}
		if err := rollup.Validate(); err != nil {
			_ = tx.Rollback()
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO metric_rollups(server_id,metric,bucket_start,bucket_seconds,sample_count,observed_seconds,minimum,maximum,weighted_mean,counter_delta,coverage) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,metric,bucket_start,bucket_seconds) DO UPDATE SET sample_count=excluded.sample_count,observed_seconds=excluded.observed_seconds,minimum=excluded.minimum,maximum=excluded.maximum,weighted_mean=excluded.weighted_mean,counter_delta=excluded.counter_delta,coverage=excluded.coverage`, string(rollup.ServerID), rollup.Metric, FormatPersistedTime(rollup.BucketStart), rollup.BucketSeconds, rollup.SampleCount, rollup.ObservedSeconds, nullableFloat(rollup.Minimum), nullableFloat(rollup.Maximum), nullableFloat(rollup.WeightedMean), rollup.CounterDelta, rollup.Coverage)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

type rollupCursor struct {
	BucketStart string `json:"bucket_start"`
	Metric      string `json:"metric"`
}

func (s *Store) QueryRollups(ctx context.Context, serverID contracts.ServerID, from, to time.Time, bucketSeconds, limit int, cursor string) ([]Rollup, string, bool, error) {
	if serverID == "" {
		return nil, "", false, errors.New("server_id is required")
	}
	if bucketSeconds != 60 && bucketSeconds != 3600 {
		return nil, "", false, errors.New("rollup bucket must be minute or hour")
	}
	// A prior ingestion may have durably accepted raw samples while the
	// derived rollup write was paused by storage pressure. Retry queued work on
	// reads so the accepted data becomes visible once headroom returns; a
	// derived-view failure must not make the durable raw history unavailable.
	if err := s.materializeRollups(ctx, serverID, nil); err != nil {
		if ctx.Err() != nil {
			return nil, "", false, ctx.Err()
		}
		// Never present stale derived data as a successful query. Raw samples
		// remain durable and the rebuild queue can be retried after headroom or
		// SQLite availability returns.
		return nil, "", false, fmt.Errorf("%w: %v", ErrRollupsPending, err)
	}
	limit = clampLimit(limit)
	decoded, err := decodeRollupCursor(cursor)
	if err != nil {
		return nil, "", false, err
	}
	query := `SELECT metric,bucket_start,sample_count,observed_seconds,minimum,maximum,weighted_mean,COALESCE(counter_delta,''),coverage FROM metric_rollups WHERE server_id=? AND bucket_seconds=?`
	args := []any{string(serverID), bucketSeconds}
	if !from.IsZero() {
		query += ` AND bucket_start >= ?`
		args = append(args, FormatPersistedTime(from))
	}
	if !to.IsZero() {
		query += ` AND bucket_start < ?`
		args = append(args, FormatPersistedTime(to))
	}
	if decoded.BucketStart != "" {
		cursorTime, err := time.Parse(time.RFC3339Nano, decoded.BucketStart)
		if err != nil {
			return nil, "", false, errors.New("invalid rollup cursor")
		}
		persistedCursor := FormatPersistedTime(cursorTime)
		query += ` AND (bucket_start > ? OR (bucket_start = ? AND metric > ?))`
		args = append(args, persistedCursor, persistedCursor, decoded.Metric)
	}
	query += ` ORDER BY bucket_start ASC, metric ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	rollups := make([]Rollup, 0, limit)
	var next string
	truncated := false
	for rows.Next() {
		var rollup Rollup
		var bucketStart, counterDelta, coverage string
		var minimum, maximum, weightedMean sql.NullFloat64
		if err := rows.Scan(&rollup.Metric, &bucketStart, &rollup.SampleCount, &rollup.ObservedSeconds, &minimum, &maximum, &weightedMean, &counterDelta, &coverage); err != nil {
			return nil, "", false, err
		}
		rollup.ServerID = serverID
		rollup.BucketStart, err = time.Parse(time.RFC3339Nano, bucketStart)
		if err != nil {
			return nil, "", false, err
		}
		if minimum.Valid {
			value := minimum.Float64
			rollup.Minimum = &value
		}
		if maximum.Valid {
			value := maximum.Float64
			rollup.Maximum = &value
		}
		if weightedMean.Valid {
			value := weightedMean.Float64
			rollup.WeightedMean = &value
		}
		rollup.BucketSeconds, rollup.CounterDelta, rollup.Coverage = bucketSeconds, counterDelta, coverage
		if err := rollup.Validate(); err != nil {
			return nil, "", false, err
		}
		if len(rollups) == limit {
			truncated = true
			last := rollups[len(rollups)-1]
			next = encodeRollupCursor(rollupCursor{BucketStart: FormatPersistedTime(last.BucketStart), Metric: last.Metric})
			break
		}
		rollups = append(rollups, rollup)
	}
	return rollups, next, truncated, rows.Err()
}

func encodeRollupCursor(cursor rollupCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeRollupCursor(value string) (rollupCursor, error) {
	if value == "" {
		return rollupCursor{}, nil
	}
	if len(value) > 256 {
		return rollupCursor{}, errors.New("invalid rollup cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return rollupCursor{}, errors.New("invalid rollup cursor")
	}
	var cursor rollupCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.BucketStart == "" || cursor.Metric == "" {
		return rollupCursor{}, errors.New("invalid rollup cursor")
	}
	return cursor, nil
}

func (s *Store) Prune(ctx context.Context, now time.Time, policy RetentionPolicy) (PruneStats, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	defaults := DefaultRetentionPolicy()
	if policy.FullResolutionAge <= 0 {
		policy.FullResolutionAge = defaults.FullResolutionAge
	}
	if policy.MinuteRollupAge <= 0 {
		policy.MinuteRollupAge = defaults.MinuteRollupAge
	}
	if policy.HourRollupAge <= 0 {
		policy.HourRollupAge = defaults.HourRollupAge
	}
	if policy.TrafficPeriodAge <= 0 {
		policy.TrafficPeriodAge = defaults.TrafficPeriodAge
	}
	if policy.LogAge <= 0 {
		policy.LogAge = defaults.LogAge
	}
	if policy.LogMaxBytes <= 0 {
		policy.LogMaxBytes = defaults.LogMaxBytes
	}
	if policy.BatchSize <= 0 || policy.BatchSize > MaxPageItems {
		policy.BatchSize = MaxPageItems
	}
	if policy.MetadataAge <= 0 {
		policy.MetadataAge = DefaultMetadataAge
	}
	if policy.MetadataMaxRows <= 0 {
		policy.MetadataMaxRows = DefaultMetadataMaxRows
	}
	var stats PruneStats
	for i := 0; i < 100; i++ {
		cutoff := FormatPersistedTime(now.Add(-policy.FullResolutionAge))
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return stats, err
		}
		selection, err := collectRetentionGaps(ctx, tx, cutoff, policy.BatchSize)
		if err != nil {
			_ = tx.Rollback()
			return stats, err
		}
		var deletedCount int64
		if len(selection.rowIDs) > 0 {
			placeholders := make([]string, len(selection.rowIDs))
			args := make([]any, len(selection.rowIDs))
			for index, rowID := range selection.rowIDs {
				placeholders[index] = "?"
				args[index] = rowID
			}
			result, err := tx.ExecContext(ctx, `DELETE FROM metric_samples WHERE rowid IN (`+strings.Join(placeholders, ",")+")", args...)
			if err != nil {
				_ = tx.Rollback()
				return stats, err
			}
			deletedCount, _ = result.RowsAffected()
			if deletedCount != int64(len(selection.rowIDs)) {
				_ = tx.Rollback()
				return stats, errors.New("retention selection changed before deletion")
			}
		}
		for _, gap := range selection.gaps {
			if err := insertCoverageGapTx(ctx, tx, gap.serverID, gap.epoch, gap.from, gap.to, "retention", gap.fromObserved, gap.toObserved); err != nil {
				_ = tx.Rollback()
				return stats, err
			}
		}
		if err := insertRetentionTombstonesTx(ctx, tx, selection.tombstones); err != nil {
			_ = tx.Rollback()
			return stats, err
		}
		for stream := range selection.frontierStreams {
			if err := advanceSequenceFrontierTx(ctx, tx, stream.serverID, stream.epoch); err != nil {
				_ = tx.Rollback()
				return stats, err
			}
		}
		if err := tx.Commit(); err != nil {
			return stats, err
		}
		stats.MetricSamples += deletedCount
		if deletedCount == 0 {
			break
		}
	}
	// Compact old per-sample ledger rows into durable range tombstones. The
	// tombstones retain replay protection after raw retention without allowing
	// the ledger itself to grow once per sample forever.
	if _, err := s.compactTrafficUsageLedger(ctx); err != nil {
		return stats, err
	}
	// Retirement-authority pressure is specific to accepting new opaque epoch
	// identities. Continue independent history/log cleanup so this fail-closed
	// condition cannot itself amplify disk pressure.
	metadataErr := s.gcCollectorEpochMetadata(ctx, now, policy.MetadataAge, policy.MetadataMaxRows)
	for _, item := range []struct {
		age   time.Duration
		value *int64
		name  string
	}{{policy.MinuteRollupAge, &stats.MinuteRollups, "minute"}, {policy.HourRollupAge, &stats.HourRollups, "hour"}} {
		for i := 0; i < 100; i++ {
			result, err := s.db.ExecContext(ctx, `DELETE FROM metric_rollups WHERE rowid IN (SELECT rowid FROM metric_rollups WHERE bucket_seconds=? AND bucket_start < ? LIMIT ?)`, map[string]int{"minute": 60, "hour": 3600}[item.name], FormatPersistedTime(now.Add(-item.age)), policy.BatchSize)
			if err != nil {
				return stats, err
			}
			count, _ := result.RowsAffected()
			*item.value += count
			if count == 0 {
				break
			}
		}
		for i := 0; i < 100; i++ {
			result, err := s.db.ExecContext(ctx, `DELETE FROM rollup_rebuild_queue WHERE rowid IN (SELECT rowid FROM rollup_rebuild_queue WHERE bucket_seconds=? AND bucket_start < ? LIMIT ?)`, map[string]int{"minute": 60, "hour": 3600}[item.name], FormatPersistedTime(now.Add(-item.age)), policy.BatchSize)
			if err != nil {
				return stats, err
			}
			count, _ := result.RowsAffected()
			if count == 0 {
				break
			}
		}
	}
	for i := 0; i < 100; i++ {
		// A queued sample can represent a delayed interval whose historical
		// allowance snapshot is available only in traffic_periods. Retain period
		// identity for any server with pending derived work; once that bounded
		// queue drains, ordinary age retention resumes.
		result, err := s.db.ExecContext(ctx, `DELETE FROM traffic_periods WHERE rowid IN (SELECT p.rowid FROM traffic_periods p WHERE p.period_end < ? AND NOT EXISTS (SELECT 1 FROM post_process_queue q WHERE q.server_id=p.server_id) LIMIT ?)`, FormatPersistedTime(now.Add(-policy.TrafficPeriodAge)), policy.BatchSize)
		if err != nil {
			return stats, err
		}
		count, _ := result.RowsAffected()
		stats.TrafficPeriods += count
		if count == 0 {
			break
		}
	}
	for i := 0; i < 100; i++ {
		result, err := s.db.ExecContext(ctx, `DELETE FROM log_entries WHERE rowid IN (SELECT rowid FROM log_entries WHERE timestamp < ? LIMIT ?)`, FormatPersistedTime(now.Add(-policy.LogAge)), policy.BatchSize)
		if err != nil {
			return stats, err
		}
		count, _ := result.RowsAffected()
		stats.LogEntries += count
		if count == 0 {
			break
		}
	}
	for i := 0; i < 100; i++ {
		result, err := s.db.ExecContext(ctx, `DELETE FROM alert_history WHERE rowid IN (SELECT rowid FROM alert_history WHERE occurred_at < ? LIMIT ?)`, FormatPersistedTime(now.Add(-policy.LogAge)), policy.BatchSize)
		if err != nil {
			return stats, err
		}
		count, _ := result.RowsAffected()
		stats.AlertHistory += count
		if count == 0 {
			break
		}
	}
	for i := 0; i < 100; i++ {
		result, err := s.db.ExecContext(ctx, `DELETE FROM incidents WHERE rowid IN (SELECT rowid FROM incidents WHERE COALESCE(ended_at,started_at) < ? AND state='recovered' LIMIT ?)`, FormatPersistedTime(now.Add(-policy.LogAge)), policy.BatchSize)
		if err != nil {
			return stats, err
		}
		count, _ := result.RowsAffected()
		stats.Incidents += count
		if count == 0 {
			break
		}
	}
	if policy.LogMaxBytes > 0 {
		for i := 0; i < 100; i++ {
			var bytes int64
			if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(CAST(text AS BLOB))),0) FROM log_entries`).Scan(&bytes); err != nil {
				return stats, err
			}
			if bytes <= policy.LogMaxBytes {
				break
			}
			result, err := s.db.ExecContext(ctx, `DELETE FROM log_entries WHERE rowid IN (SELECT rowid FROM log_entries ORDER BY timestamp ASC, rowid ASC LIMIT ?)`, policy.BatchSize)
			if err != nil {
				return stats, err
			}
			count, _ := result.RowsAffected()
			stats.LogEntries += count
			if count == 0 {
				break
			}
		}
	}
	return stats, metadataErr
}

// gcCollectorEpochMetadata retires inactive epoch metadata after raw samples
// and all package-specific work have disappeared. The age window is explicit
// because the deleted tombstones are also the replay-protection window. A
// fleet-wide row cap is a second safety valve; it removes only the oldest
// epochs that have no raw sample, post-process queue entry, or usage ledger
// row. Every table deletion is in one transaction, and queued samples are
// never deleted by this cleanup.
func (s *Store) gcCollectorEpochMetadata(ctx context.Context, now time.Time, maxAge time.Duration, maxRows int) error {
	if maxAge <= 0 {
		maxAge = DefaultMetadataAge
	}
	if maxRows <= 0 {
		maxRows = DefaultMetadataMaxRows
	}
	cutoff := FormatPersistedTime(now.Add(-maxAge))
	for attempt := 0; attempt < 100; attempt++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		retirementLimit, retirementRows, err := configureCollectorEpochRetirementAuthorityTx(ctx, tx, maxRows)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		total, err := countCollectorEpochMetadataRowsTx(ctx, tx)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		query := `SELECT e.server_id,e.collector_epoch FROM collector_epoch_metadata e WHERE e.last_seen_at < ? AND NOT EXISTS (SELECT 1 FROM metric_samples m WHERE m.server_id=e.server_id AND m.collector_epoch=e.collector_epoch) AND NOT EXISTS (SELECT 1 FROM post_process_queue q WHERE q.server_id=e.server_id AND q.collector_epoch=e.collector_epoch) AND NOT EXISTS (SELECT 1 FROM traffic_usage_ledger l WHERE l.server_id=e.server_id AND l.collector_epoch=e.collector_epoch) ORDER BY e.last_seen_at ASC,e.server_id ASC,e.collector_epoch ASC LIMIT ?`
		args := []any{cutoff, MaxPageItems}
		if total <= int64(maxRows) {
			// The age predicate is enough while metadata remains under the cap.
			// Keeping this branch explicit makes the cap behavior below easier to
			// audit: stale rows are still retired, but active streams are not.
		} else {
			// The cap may retire recent idle epochs, but never the current
			// epoch for a server. A stream with no raw/queued/ledger work is
			// otherwise safe to retire; preserving its most recently observed
			// metadata keeps an active gap-only collector from losing its
			// frontier merely because another server has many stale epochs.
			query = `SELECT e.server_id,e.collector_epoch FROM collector_epoch_metadata e WHERE (e.last_seen_at < ? OR NOT (e.last_seen_at >= ? AND NOT EXISTS (SELECT 1 FROM collector_epoch_metadata current_epoch WHERE current_epoch.server_id=e.server_id AND (current_epoch.last_seen_at > e.last_seen_at OR (current_epoch.last_seen_at=e.last_seen_at AND current_epoch.collector_epoch > e.collector_epoch))))) AND NOT EXISTS (SELECT 1 FROM metric_samples m WHERE m.server_id=e.server_id AND m.collector_epoch=e.collector_epoch) AND NOT EXISTS (SELECT 1 FROM post_process_queue q WHERE q.server_id=e.server_id AND q.collector_epoch=e.collector_epoch) AND NOT EXISTS (SELECT 1 FROM traffic_usage_ledger l WHERE l.server_id=e.server_id AND l.collector_epoch=e.collector_epoch) ORDER BY e.last_seen_at ASC,e.server_id ASC,e.collector_epoch ASC LIMIT ?`
			args = []any{cutoff, cutoff, MaxPageItems}
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		type epochIdentity struct {
			serverID string
			epoch    string
		}
		candidates := make([]epochIdentity, 0, MaxPageItems)
		for rows.Next() {
			var candidate epochIdentity
			if err := rows.Scan(&candidate.serverID, &candidate.epoch); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return err
			}
			candidates = append(candidates, candidate)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return err
		}
		if err := rows.Close(); err != nil {
			_ = tx.Rollback()
			return err
		}
		if len(candidates) == 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			return nil
		}
		capacity := retirementLimit - retirementRows
		blockedByAuthority := int64(len(candidates)) > capacity
		if capacity < int64(len(candidates)) {
			if capacity <= 0 {
				candidates = nil
			} else {
				candidates = candidates[:int(capacity)]
			}
		}
		for _, candidate := range candidates {
			// Commit the authority before removing any range tombstones. The
			// marker makes a later replay fail closed even after all per-range
			// metadata for this retired epoch has been compacted away.
			if _, err := tx.ExecContext(ctx, `INSERT INTO collector_epoch_retirements(server_id,collector_epoch,retired_at) VALUES(?,?,?) ON CONFLICT DO NOTHING`, candidate.serverID, candidate.epoch, FormatPersistedTime(now)); err != nil {
				_ = tx.Rollback()
				return err
			}
			for _, table := range []string{"coverage_gaps", "metric_sample_tombstones", "traffic_usage_tombstones", "sequence_frontiers"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE server_id=? AND collector_epoch=?`, candidate.serverID, candidate.epoch); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM collector_epoch_metadata WHERE server_id=? AND collector_epoch=?`, candidate.serverID, candidate.epoch); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collector_epoch_retirement_authority SET saturated=CASE WHEN (SELECT COUNT(*) FROM collector_epoch_retirements) >= max_rows THEN 1 ELSE 0 END WHERE singleton=1`); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if blockedByAuthority {
			return ErrCollectorEpochAuthorityFull
		}
		if total <= int64(maxRows) && len(candidates) < MaxPageItems {
			return nil
		}
	}
	return nil
}

// configureCollectorEpochRetirementAuthorityTx applies the requested bound to
// future growth. A database upgraded with more exact retirement markers than
// today's configured limit keeps that high-water mark: deleting those opaque
// identities would weaken replay protection. It is immediately saturated, so
// no additional unseen epoch can enter until the configured limit is raised.
func configureCollectorEpochRetirementAuthorityTx(ctx context.Context, tx *sql.Tx, requested int) (limit, rows int64, err error) {
	if tx == nil {
		return 0, 0, errors.New("retirement authority transaction is required")
	}
	if requested <= 0 {
		requested = DefaultMetadataMaxRows
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM collector_epoch_retirements`).Scan(&rows); err != nil {
		return 0, 0, err
	}
	limit = int64(requested)
	if rows > limit {
		limit = rows
	}
	_, err = tx.ExecContext(ctx, `UPDATE collector_epoch_retirement_authority SET max_rows=?,saturated=CASE WHEN ?>= ? THEN 1 ELSE 0 END WHERE singleton=1`, limit, rows, limit)
	return limit, rows, err
}

func countCollectorEpochMetadataRowsTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	if tx == nil {
		return 0, errors.New("metadata transaction is required")
	}
	var total int64
	for _, table := range []string{"collector_epoch_metadata", "collector_epoch_retirements", "sequence_frontiers", "coverage_gaps", "metric_sample_tombstones", "traffic_usage_tombstones", "traffic_usage_ledger"} {
		var count int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

type usageLedgerEntry struct {
	serverID  contracts.ServerID
	epoch     contracts.CollectorEpoch
	scope     string
	direction string
	sequence  uint64
}

// compactTrafficUsageLedger moves a bounded number of ledger rows whose raw
// sample and post-process queue entry are both gone into inclusive ranges. A
// tombstone is inserted and the source rows are deleted in the same
// transaction, so a crash cannot create a replay window.
func (s *Store) compactTrafficUsageLedger(ctx context.Context) (int64, error) {
	var compacted int64
	for attempt := 0; attempt < 100; attempt++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return compacted, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT l.server_id,l.collector_epoch,l.sequence,l.scope,l.direction FROM traffic_usage_ledger l WHERE NOT EXISTS (SELECT 1 FROM metric_samples m WHERE m.server_id=l.server_id AND m.collector_epoch=l.collector_epoch AND m.sequence=l.sequence) AND NOT EXISTS (SELECT 1 FROM post_process_queue q WHERE q.server_id=l.server_id AND q.collector_epoch=l.collector_epoch AND q.sequence=l.sequence) ORDER BY l.server_id,l.collector_epoch,l.scope,l.direction,length(l.sequence),l.sequence LIMIT ?`, MaxPageItems)
		if err != nil {
			_ = tx.Rollback()
			return compacted, err
		}
		entries := make([]usageLedgerEntry, 0, MaxPageItems)
		for rows.Next() {
			var serverID, epoch, sequence, scope, direction string
			if err := rows.Scan(&serverID, &epoch, &sequence, &scope, &direction); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return compacted, err
			}
			parsed, err := strconv.ParseUint(sequence, 10, 64)
			if err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return compacted, errors.New("invalid persisted traffic usage sequence")
			}
			entries = append(entries, usageLedgerEntry{serverID: contracts.ServerID(serverID), epoch: contracts.CollectorEpoch(epoch), scope: scope, direction: direction, sequence: parsed})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return compacted, err
		}
		if err := rows.Close(); err != nil {
			_ = tx.Rollback()
			return compacted, err
		}
		if len(entries) == 0 {
			if err := tx.Commit(); err != nil {
				return compacted, err
			}
			return compacted, nil
		}
		flush := func(start, end usageLedgerEntry) error {
			return insertTrafficUsageTombstoneTx(ctx, tx, start.serverID, start.epoch, start.scope, start.direction, start.sequence, end.sequence)
		}
		start := entries[0]
		end := start
		for _, entry := range entries[1:] {
			sameStream := entry.serverID == end.serverID && entry.epoch == end.epoch && entry.scope == end.scope && entry.direction == end.direction
			contiguous := end.sequence != ^uint64(0) && entry.sequence == end.sequence+1
			if sameStream && contiguous {
				end = entry
				continue
			}
			if err := flush(start, end); err != nil {
				_ = tx.Rollback()
				return compacted, err
			}
			start, end = entry, entry
		}
		if err := flush(start, end); err != nil {
			_ = tx.Rollback()
			return compacted, err
		}
		for _, entry := range entries {
			if _, err := tx.ExecContext(ctx, `DELETE FROM traffic_usage_ledger WHERE server_id=? AND collector_epoch=? AND sequence=? AND scope=? AND direction=?`, string(entry.serverID), string(entry.epoch), strconv.FormatUint(entry.sequence, 10), entry.scope, entry.direction); err != nil {
				_ = tx.Rollback()
				return compacted, err
			}
		}
		if err := tx.Commit(); err != nil {
			return compacted, err
		}
		compacted += int64(len(entries))
		if len(entries) < MaxPageItems {
			return compacted, nil
		}
	}
	return compacted, nil
}

type retentionGap struct {
	serverID     contracts.ServerID
	epoch        contracts.CollectorEpoch
	from         uint64
	to           uint64
	fromObserved string
	toObserved   string
}

type retentionTombstone struct {
	serverID contracts.ServerID
	epoch    contracts.CollectorEpoch
	from     uint64
	to       uint64 // inclusive
}

type retentionStream struct {
	serverID contracts.ServerID
	epoch    contracts.CollectorEpoch
}

type retentionSelection struct {
	gaps            []retentionGap
	tombstones      []retentionTombstone
	rowIDs          []int64
	frontierStreams map[retentionStream]struct{}
}

// insertCoverageGapTx inserts a half-open coverage range and coalesces every
// overlapping or adjacent range for the same server/epoch. The range tables
// store uint64 values as canonical decimal strings, so the merge is performed
// after parsing and sorting numerically rather than relying on SQLite's text
// ordering. Existing rows are removed and the merged row is written in this
// transaction; a rollback therefore cannot leave a replay/coverage window.
func insertCoverageGapTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, from, to uint64, reason, fromObserved, toObserved string) error {
	if tx == nil {
		return errors.New("coverage transaction is required")
	}
	if from >= to {
		return errors.New("invalid coverage gap range")
	}
	if !validCoverageGapReason(reason) {
		return errors.New("invalid coverage gap reason")
	}

	rows, err := tx.QueryContext(ctx, `SELECT from_sequence,to_sequence,reason,COALESCE(from_observed_at,''),COALESCE(to_observed_at,'') FROM coverage_gaps WHERE server_id=? AND collector_epoch=? LIMIT ?`, string(serverID), string(epoch), MaxRangeMetadataRows+1)
	if err != nil {
		return err
	}
	type coverageGapRange struct {
		from, to                 uint64
		reason                   string
		fromObserved, toObserved string
		incoming                 bool
	}
	ranges := make([]coverageGapRange, 0)
	for rows.Next() {
		var fromText, toText, existingReason, existingFromObserved, existingToObserved string
		if err := rows.Scan(&fromText, &toText, &existingReason, &existingFromObserved, &existingToObserved); err != nil {
			_ = rows.Close()
			return err
		}
		existingFrom, parseErr := parsePersistedSequence(fromText)
		if parseErr != nil {
			_ = rows.Close()
			return parseErr
		}
		existingTo, parseErr := parsePersistedSequence(toText)
		if parseErr != nil {
			_ = rows.Close()
			return parseErr
		}
		if existingFrom >= existingTo {
			_ = rows.Close()
			return errors.New("invalid persisted coverage gap range")
		}
		ranges = append(ranges, coverageGapRange{from: existingFrom, to: existingTo, reason: existingReason, fromObserved: existingFromObserved, toObserved: existingToObserved})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(ranges) > MaxRangeMetadataRows {
		return ErrRangeMetadataFull
	}
	ranges = append(ranges, coverageGapRange{from: from, to: to, reason: reason, fromObserved: fromObserved, toObserved: toObserved, incoming: true})
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].from != ranges[j].from {
			return ranges[i].from < ranges[j].from
		}
		if ranges[i].to != ranges[j].to {
			return ranges[i].to < ranges[j].to
		}
		if ranges[i].reason != ranges[j].reason {
			return ranges[i].reason < ranges[j].reason
		}
		return !ranges[i].incoming && ranges[j].incoming
	})

	var merged coverageGapRange
	matched := make([]coverageGapRange, 0)
	for index := 0; index < len(ranges); {
		cluster := ranges[index]
		clusterRows := []coverageGapRange{ranges[index]}
		index++
		for index < len(ranges) && halfOpenRangesTouch(cluster.from, cluster.to, ranges[index].from, ranges[index].to) {
			cluster = mergeCoverageGapRange(cluster, ranges[index])
			clusterRows = append(clusterRows, ranges[index])
			index++
		}
		if !cluster.incoming {
			continue
		}
		merged = cluster
		for _, existing := range clusterRows {
			if !existing.incoming {
				matched = append(matched, existing)
			}
		}
		break
	}
	if len(matched) == 0 {
		if err := ensureRangeMetadataCapacityTx(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO coverage_gaps(server_id,collector_epoch,from_sequence,to_sequence,reason,from_observed_at,to_observed_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, string(serverID), string(epoch), strconv.FormatUint(from, 10), strconv.FormatUint(to, 10), reason, nullableString(fromObserved), nullableString(toObserved))
		return err
	}
	for _, existing := range matched {
		if _, err := tx.ExecContext(ctx, `DELETE FROM coverage_gaps WHERE server_id=? AND collector_epoch=? AND from_sequence=? AND to_sequence=? AND reason=?`, string(serverID), string(epoch), strconv.FormatUint(existing.from, 10), strconv.FormatUint(existing.to, 10), existing.reason); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO coverage_gaps(server_id,collector_epoch,from_sequence,to_sequence,reason,from_observed_at,to_observed_at) VALUES(?,?,?,?,?,?,?)`, string(serverID), string(epoch), strconv.FormatUint(merged.from, 10), strconv.FormatUint(merged.to, 10), merged.reason, nullableString(merged.fromObserved), nullableString(merged.toObserved))
	return err
}

func mergeCoverageGapRange(left, right struct {
	from, to                 uint64
	reason                   string
	fromObserved, toObserved string
	incoming                 bool
}) struct {
	from, to                 uint64
	reason                   string
	fromObserved, toObserved string
	incoming                 bool
} {
	if right.from < left.from {
		left.from = right.from
	}
	if right.to > left.to {
		left.to = right.to
	}
	left.reason = mergeCoverageGapReason(left.reason, right.reason)
	left.fromObserved = earliestObserved(left.fromObserved, right.fromObserved)
	left.toObserved = latestObserved(left.toObserved, right.toObserved)
	left.incoming = left.incoming || right.incoming
	return left
}

func validCoverageGapReason(reason string) bool {
	switch reason {
	case "spool-eviction", "transport-disconnect", "out-of-order", "retention":
		return true
	default:
		return false
	}
}

// Retention is kept as the aggregate reason when a locally-created retention
// range touches an explicit transport range. For two non-retention causes the
// lexical choice is stable, which keeps cursor ordering deterministic even
// when batches arrive in a different order.
func mergeCoverageGapReason(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" || left == right {
		return left
	}
	if left == "retention" || right == "retention" {
		return "retention"
	}
	if right < left {
		return right
	}
	return left
}

func earliestObserved(left, right string) string {
	// A missing side is intentionally sticky. Query filters treat NULL as
	// potentially overlapping; filling it from an adjacent range could hide
	// uncertainty at the edge of the coalesced coverage interval.
	if left == "" || right == "" {
		return ""
	}
	if left <= right {
		return left
	}
	return right
}

func latestObserved(left, right string) string {
	// See earliestObserved: unknown bounds must remain unknown after merging.
	if left == "" || right == "" {
		return ""
	}
	if left >= right {
		return left
	}
	return right
}

func parsePersistedSequence(value string) (uint64, error) {
	if !decimalCounterString(value) {
		return 0, errors.New("invalid persisted sequence")
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errors.New("invalid persisted sequence")
	}
	return parsed, nil
}

// Half-open ranges [from,to) touch when they overlap or share an endpoint.
// The endpoint representation intentionally never adds one, so MaxUint64
// cannot overflow while deciding adjacency.
func halfOpenRangesTouch(leftFrom, leftTo, rightFrom, rightTo uint64) bool {
	if leftFrom >= leftTo || rightFrom >= rightTo {
		return false
	}
	return leftFrom <= rightTo && rightFrom <= leftTo
}

// Inclusive ranges [from,to] touch when they overlap or are directly
// adjacent. Both sides guard the +1 operation so MaxUint64 remains safe.
func inclusiveRangesTouch(leftFrom, leftTo, rightFrom, rightTo uint64) bool {
	const maxSequence = ^uint64(0)
	if leftFrom > leftTo || rightFrom > rightTo {
		return false
	}
	if leftTo != maxSequence && rightFrom > leftTo+1 {
		return false
	}
	if rightTo != maxSequence && leftFrom > rightTo+1 {
		return false
	}
	return true
}

type metricSampleTombstoneRange struct {
	from, to uint64
	incoming bool
}

// insertMetricSampleTombstoneTx coalesces inclusive raw-sample replay ranges
// for one server/epoch. It is intentionally transaction-scoped because the
// caller deletes the corresponding raw sample in the same transaction.
func insertMetricSampleTombstoneTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, from, to uint64) error {
	if tx == nil {
		return errors.New("tombstone transaction is required")
	}
	if from > to {
		return errors.New("invalid retention tombstone range")
	}
	rows, err := tx.QueryContext(ctx, `SELECT from_sequence,to_sequence FROM metric_sample_tombstones WHERE server_id=? AND collector_epoch=? LIMIT ?`, string(serverID), string(epoch), MaxRangeMetadataRows+1)
	if err != nil {
		return err
	}
	ranges := make([]metricSampleTombstoneRange, 0)
	for rows.Next() {
		var fromText, toText string
		if err := rows.Scan(&fromText, &toText); err != nil {
			_ = rows.Close()
			return err
		}
		existingFrom, parseErr := parsePersistedSequence(fromText)
		if parseErr != nil {
			_ = rows.Close()
			return parseErr
		}
		existingTo, parseErr := parsePersistedSequence(toText)
		if parseErr != nil {
			_ = rows.Close()
			return parseErr
		}
		if existingFrom > existingTo {
			_ = rows.Close()
			return errors.New("invalid persisted metric sample tombstone range")
		}
		ranges = append(ranges, metricSampleTombstoneRange{from: existingFrom, to: existingTo})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(ranges) > MaxRangeMetadataRows {
		return ErrRangeMetadataFull
	}
	ranges = append(ranges, metricSampleTombstoneRange{from: from, to: to, incoming: true})
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].from != ranges[j].from {
			return ranges[i].from < ranges[j].from
		}
		if ranges[i].to != ranges[j].to {
			return ranges[i].to < ranges[j].to
		}
		return !ranges[i].incoming && ranges[j].incoming
	})
	var merged metricSampleTombstoneRange
	matched := make([]metricSampleTombstoneRange, 0)
	for index := 0; index < len(ranges); {
		cluster := ranges[index]
		clusterRows := []metricSampleTombstoneRange{ranges[index]}
		index++
		for index < len(ranges) && inclusiveRangesTouch(cluster.from, cluster.to, ranges[index].from, ranges[index].to) {
			if ranges[index].from < cluster.from {
				cluster.from = ranges[index].from
			}
			if ranges[index].to > cluster.to {
				cluster.to = ranges[index].to
			}
			cluster.incoming = cluster.incoming || ranges[index].incoming
			clusterRows = append(clusterRows, ranges[index])
			index++
		}
		if !cluster.incoming {
			continue
		}
		merged = cluster
		for _, existing := range clusterRows {
			if !existing.incoming {
				matched = append(matched, existing)
			}
		}
		break
	}
	if len(matched) == 0 {
		if err := ensureRangeMetadataCapacityTx(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO metric_sample_tombstones(server_id,collector_epoch,from_sequence,to_sequence) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, string(serverID), string(epoch), strconv.FormatUint(from, 10), strconv.FormatUint(to, 10))
		return err
	}
	for _, existing := range matched {
		if _, err := tx.ExecContext(ctx, `DELETE FROM metric_sample_tombstones WHERE server_id=? AND collector_epoch=? AND from_sequence=? AND to_sequence=?`, string(serverID), string(epoch), strconv.FormatUint(existing.from, 10), strconv.FormatUint(existing.to, 10)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO metric_sample_tombstones(server_id,collector_epoch,from_sequence,to_sequence) VALUES(?,?,?,?)`, string(serverID), string(epoch), strconv.FormatUint(merged.from, 10), strconv.FormatUint(merged.to, 10))
	return err
}

type trafficUsageTombstoneRange struct {
	from, to uint64
	incoming bool
}

// insertTrafficUsageTombstoneTx coalesces inclusive usage replay ranges for
// one allowance stream. Like the metric tombstones, source-ledger deletion is
// performed by the caller in this same transaction.
func insertTrafficUsageTombstoneTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, scope, direction string, from, to uint64) error {
	if tx == nil {
		return errors.New("tombstone transaction is required")
	}
	if from > to {
		return errors.New("invalid usage tombstone range")
	}
	rows, err := tx.QueryContext(ctx, `SELECT from_sequence,to_sequence FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope=? AND direction=? LIMIT ?`, string(serverID), string(epoch), scope, direction, MaxRangeMetadataRows+1)
	if err != nil {
		return err
	}
	ranges := make([]trafficUsageTombstoneRange, 0)
	for rows.Next() {
		var fromText, toText string
		if err := rows.Scan(&fromText, &toText); err != nil {
			_ = rows.Close()
			return err
		}
		existingFrom, parseErr := parsePersistedSequence(fromText)
		if parseErr != nil {
			_ = rows.Close()
			return parseErr
		}
		existingTo, parseErr := parsePersistedSequence(toText)
		if parseErr != nil {
			_ = rows.Close()
			return parseErr
		}
		if existingFrom > existingTo {
			_ = rows.Close()
			return errors.New("invalid persisted traffic usage tombstone range")
		}
		ranges = append(ranges, trafficUsageTombstoneRange{from: existingFrom, to: existingTo})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(ranges) > MaxRangeMetadataRows {
		return ErrRangeMetadataFull
	}
	ranges = append(ranges, trafficUsageTombstoneRange{from: from, to: to, incoming: true})
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].from != ranges[j].from {
			return ranges[i].from < ranges[j].from
		}
		if ranges[i].to != ranges[j].to {
			return ranges[i].to < ranges[j].to
		}
		return !ranges[i].incoming && ranges[j].incoming
	})
	var merged trafficUsageTombstoneRange
	matched := make([]trafficUsageTombstoneRange, 0)
	for index := 0; index < len(ranges); {
		cluster := ranges[index]
		clusterRows := []trafficUsageTombstoneRange{ranges[index]}
		index++
		for index < len(ranges) && inclusiveRangesTouch(cluster.from, cluster.to, ranges[index].from, ranges[index].to) {
			if ranges[index].from < cluster.from {
				cluster.from = ranges[index].from
			}
			if ranges[index].to > cluster.to {
				cluster.to = ranges[index].to
			}
			cluster.incoming = cluster.incoming || ranges[index].incoming
			clusterRows = append(clusterRows, ranges[index])
			index++
		}
		if !cluster.incoming {
			continue
		}
		merged = cluster
		for _, existing := range clusterRows {
			if !existing.incoming {
				matched = append(matched, existing)
			}
		}
		break
	}
	if len(matched) == 0 {
		if err := ensureRangeMetadataCapacityTx(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO traffic_usage_tombstones(server_id,collector_epoch,scope,direction,from_sequence,to_sequence) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, string(serverID), string(epoch), scope, direction, strconv.FormatUint(from, 10), strconv.FormatUint(to, 10))
		return err
	}
	for _, existing := range matched {
		if _, err := tx.ExecContext(ctx, `DELETE FROM traffic_usage_tombstones WHERE server_id=? AND collector_epoch=? AND scope=? AND direction=? AND from_sequence=? AND to_sequence=?`, string(serverID), string(epoch), scope, direction, strconv.FormatUint(existing.from, 10), strconv.FormatUint(existing.to, 10)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO traffic_usage_tombstones(server_id,collector_epoch,scope,direction,from_sequence,to_sequence) VALUES(?,?,?,?,?,?)`, string(serverID), string(epoch), scope, direction, strconv.FormatUint(merged.from, 10), strconv.FormatUint(merged.to, 10))
	return err
}

func ensureRangeMetadataCapacityTx(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("range metadata transaction is required")
	}
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM coverage_gaps)+(SELECT COUNT(*) FROM metric_sample_tombstones)+(SELECT COUNT(*) FROM traffic_usage_tombstones)`).Scan(&rows); err != nil {
		return err
	}
	if rows >= MaxRangeMetadataRows {
		return ErrRangeMetadataFull
	}
	return nil
}

func insertRetentionTombstonesTx(ctx context.Context, tx *sql.Tx, tombstones []retentionTombstone) error {
	for _, tombstone := range tombstones {
		if err := insertMetricSampleTombstoneTx(ctx, tx, tombstone.serverID, tombstone.epoch, tombstone.from, tombstone.to); err != nil {
			return err
		}
	}
	return nil
}

func collectRetentionGaps(ctx context.Context, tx *sql.Tx, cutoff string, limit int) (retentionSelection, error) {
	// A queued sample is the authoritative payload for a post-commit retry.
	// Keep its raw row until the observer acknowledges it: traffic/alert
	// processing may need the immediately preceding raw sample to establish a
	// counter delta. This protects both the queued row itself and the nearest
	// prior raw row for every queued sequence; the latter may already have been
	// acknowledged while the newer sample is still waiting. The queue and
	// retention delete both run through SQLite's single transaction, so this
	// predicate cannot race an acknowledgement.
	rows, err := tx.QueryContext(ctx, `SELECT m.rowid,m.server_id,m.collector_epoch,m.sequence,m.observed_at
FROM metric_samples m
WHERE m.observed_at < ?
  AND NOT EXISTS (
    SELECT 1
    FROM post_process_queue q
    WHERE q.server_id=m.server_id
      AND q.collector_epoch=m.collector_epoch
      AND (
        q.sequence=m.sequence
        OR (
          (length(q.sequence) > length(m.sequence) OR (length(q.sequence)=length(m.sequence) AND q.sequence > m.sequence))
          AND NOT EXISTS (
            SELECT 1
            FROM metric_samples p
            WHERE p.server_id=m.server_id
              AND p.collector_epoch=m.collector_epoch
              AND (length(p.sequence) > length(m.sequence) OR (length(p.sequence)=length(m.sequence) AND p.sequence > m.sequence))
              AND (length(p.sequence) < length(q.sequence) OR (length(p.sequence)=length(q.sequence) AND p.sequence < q.sequence))
          )
        )
      )
  )
ORDER BY m.observed_at ASC, m.rowid ASC LIMIT ?`, cutoff, limit)
	if err != nil {
		return retentionSelection{}, err
	}
	defer rows.Close()
	type key struct {
		serverID string
		epoch    string
	}
	type retainedSample struct {
		sequence   uint64
		observedAt string
	}
	selectedByKey := make(map[key][]retainedSample)
	selection := retentionSelection{rowIDs: make([]int64, 0, limit), frontierStreams: make(map[retentionStream]struct{})}
	for rows.Next() {
		var rowID int64
		var serverID, epoch, sequence, observedAt string
		if err := rows.Scan(&rowID, &serverID, &epoch, &sequence, &observedAt); err != nil {
			return retentionSelection{}, err
		}
		if !decimalCounterString(sequence) {
			return retentionSelection{}, errors.New("invalid retained metric sequence")
		}
		selection.rowIDs = append(selection.rowIDs, rowID)
		identity := key{serverID: serverID, epoch: epoch}
		selection.frontierStreams[retentionStream{serverID: contracts.ServerID(serverID), epoch: contracts.CollectorEpoch(epoch)}] = struct{}{}
		parsed, err := strconv.ParseUint(sequence, 10, 64)
		if err != nil {
			return retentionSelection{}, err
		}
		selectedByKey[identity] = append(selectedByKey[identity], retainedSample{sequence: parsed, observedAt: observedAt})
	}
	gaps := make([]retentionGap, 0, len(selection.rowIDs))
	tombstones := make([]retentionTombstone, 0, len(selection.rowIDs))
	for identity, selected := range selectedByKey {
		sort.Slice(selected, func(i, j int) bool { return selected[i].sequence < selected[j].sequence })
		for start := 0; start < len(selected); {
			end := start
			for end+1 < len(selected) && selected[end].sequence != ^uint64(0) && selected[end+1].sequence == selected[end].sequence+1 {
				end++
			}
			from, last := selected[start].sequence, selected[end].sequence
			tombstones = append(tombstones, retentionTombstone{serverID: contracts.ServerID(identity.serverID), epoch: contracts.CollectorEpoch(identity.epoch), from: from, to: last})
			// Coverage gaps are half-open. A run ending at MaxUint64 still
			// contributes the representable prefix [from, MaxUint64); the
			// singleton MaxUint64 itself has no successor and is tracked by the
			// frontier's max_point bit instead.
			if last == ^uint64(0) {
				if from < last {
					fromObserved, toObserved := selected[start].observedAt, selected[start].observedAt
					for i := start + 1; i < end; i++ {
						if selected[i].observedAt < fromObserved {
							fromObserved = selected[i].observedAt
						}
						if selected[i].observedAt > toObserved {
							toObserved = selected[i].observedAt
						}
					}
					gaps = append(gaps, retentionGap{serverID: contracts.ServerID(identity.serverID), epoch: contracts.CollectorEpoch(identity.epoch), from: from, to: last, fromObserved: fromObserved, toObserved: toObserved})
				}
			} else {
				fromObserved, toObserved := selected[start].observedAt, selected[start].observedAt
				for i := start + 1; i <= end; i++ {
					if selected[i].observedAt < fromObserved {
						fromObserved = selected[i].observedAt
					}
					if selected[i].observedAt > toObserved {
						toObserved = selected[i].observedAt
					}
				}
				gaps = append(gaps, retentionGap{serverID: contracts.ServerID(identity.serverID), epoch: contracts.CollectorEpoch(identity.epoch), from: from, to: last + 1, fromObserved: fromObserved, toObserved: toObserved})
			}
			start = end + 1
		}
	}
	selection.gaps = gaps
	selection.tombstones = tombstones
	return selection, rows.Err()
}

func decimalCounterString(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func decimalLess(left, right string) bool {
	if len(left) != len(right) {
		return len(left) < len(right)
	}
	return left < right
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > MaxPageItems {
		return MaxPageItems
	}
	return limit
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
