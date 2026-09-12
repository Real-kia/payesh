package transport

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/Real-kia/payesh/internal/contracts"
)

// MaxSpoolBytes is the default and maximum supported node-side offline spool.
// The bound is intentionally small enough to protect a constrained VPS while
// still allowing batches to be written atomically.
const MaxSpoolBytes int64 = 32 << 20

const maxSpoolRecordBytes = MaxSpoolBytes

var (
	ErrSpoolCorrupt      = errors.New("transport: offline spool is corrupt")
	ErrSpoolRecordTooBig = errors.New("transport: offline spool record exceeds its bound")
	ErrSpoolPathInvalid  = errors.New("transport: offline spool path must be absolute and clean")
	ErrSpoolBatchInvalid = errors.New("transport: offline spool batch is invalid")
)

// SpoolRecord is one durable resend unit. A record is always one collector
// epoch so a cumulative acknowledgement can never advance across a restart.
type SpoolRecord struct {
	Batch contracts.SampleBatch
}

type storedSpoolRecord struct {
	Batch contracts.SampleBatch `json:"batch"`
}

// Spool is a bounded, crash-safe append-only queue. Mutations rewrite the
// compacted file through a same-directory temporary file and rename, so a
// process crash leaves either the previous complete queue or the new one.
type Spool struct {
	path    string
	maxSize int64
	mu      sync.Mutex
}

// OpenSpool opens an existing queue or creates an empty one. The path must be
// an absolute clean path; callers should place it under the agent data root.
func OpenSpool(path string, maxBytes int64) (*Spool, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrSpoolPathInvalid
	}
	if maxBytes <= 0 {
		maxBytes = MaxSpoolBytes
	}
	if maxBytes > MaxSpoolBytes {
		return nil, fmt.Errorf("transport: spool bound %d exceeds maximum %d", maxBytes, MaxSpoolBytes)
	}
	s := &Spool{path: path, maxSize: maxBytes}
	if _, err := os.Stat(path); err == nil {
		if _, err := s.read(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("transport: inspect spool: %w", err)
	}
	return s, nil
}

// Path returns the configured queue path for diagnostics.
func (s *Spool) Path() string { return s.path }

// Append durably queues a batch. If older records must be evicted to remain
// within the byte bound, an explicit coverage-gap record is written before
// the remaining samples in the same atomic rewrite. The returned gaps are
// retained for diagnostics/backward-compatible callers; replay need not keep
// them in process memory.
func (s *Spool) Append(batch contracts.SampleBatch) ([]contracts.CoverageGap, error) {
	if err := validateSpoolBatch(batch); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.read()
	if err != nil {
		return nil, err
	}
	records = append(records, storedSpoolRecord{Batch: batch})
	if encoded, err := encodeSpool(records[len(records)-1]); err != nil {
		return nil, err
	} else if int64(4+len(encoded)) > s.maxSize {
		return nil, ErrSpoolRecordTooBig
	}
	var evicted []storedSpoolRecord
	for spoolSize(records) > s.maxSize && len(records) > 0 {
		evicted = append(evicted, records[0])
		records = records[1:]
	}
	// Persist the explicit coverage created by eviction in the same atomic
	// rewrite as the remaining samples. Keeping it only in the caller would
	// lose the evidence on an agent crash between Append and reconnect.
	gaps := evictionGaps(evicted)
	if len(gaps) > 0 {
		gapRecords := durableGapRecords(gaps)
		tail := records
		records = append(gapRecords, tail...)
		// The gap record is deliberately kept at the front. If adding it puts
		// the queue over a very small configured bound, evict the oldest sample
		// after the gap and fold its coverage into the same durable record.
		for spoolSize(records) > s.maxSize && len(tail) > 0 {
			victim := tail[0]
			tail = tail[1:]
			gaps = append(gaps, evictionGaps([]storedSpoolRecord{victim})...)
			gaps = evictionGapsFromGaps(gaps)
			gapRecords = durableGapRecords(gaps)
			records = append(gapRecords, tail...)
		}
		if spoolSize(records) > s.maxSize {
			return nil, ErrSpoolRecordTooBig
		}
	}
	if err := s.write(records); err != nil {
		return nil, err
	}
	return gaps, nil
}

// Pending returns a snapshot in FIFO order. A non-positive limit means all
// records. Returned batches are copied so callers can safely mutate them.
func (s *Spool) Pending(limit int) ([]SpoolRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.read()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > len(records) {
		limit = len(records)
	}
	out := make([]SpoolRecord, 0, limit)
	for _, record := range records[:limit] {
		out = append(out, SpoolRecord{Batch: cloneBatch(record.Batch)})
	}
	return out, nil
}

// Acknowledge removes coverage at or before through for one collector epoch.
// Partial records are retained with their unacknowledged suffix. An unknown
// epoch is a safe no-op, which is useful during reconnect races.
func (s *Spool) Acknowledge(epoch contracts.CollectorEpoch, through uint64) error {
	if epoch == "" {
		return ErrSpoolBatchInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.read()
	if err != nil {
		return err
	}
	changed := false
	kept := make([]storedSpoolRecord, 0, len(records))
	for _, record := range records {
		batch, removed := acknowledgeBatch(record.Batch, epoch, through)
		if removed {
			changed = true
		}
		if len(batch.Samples) != 0 || len(batch.Gaps) != 0 {
			kept = append(kept, storedSpoolRecord{Batch: batch})
		} else if len(record.Batch.Samples) != 0 || len(record.Batch.Gaps) != 0 {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.write(kept)
}

func validateSpoolBatch(batch contracts.SampleBatch) error {
	if len(batch.Samples) == 0 && len(batch.Gaps) == 0 {
		return ErrSpoolBatchInvalid
	}
	if err := batch.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrSpoolBatchInvalid, err)
	}
	if err := validateBatchEpoch(batch); err != nil {
		return fmt.Errorf("%w: %v", ErrSpoolBatchInvalid, err)
	}
	return nil
}

func (s *Spool) read() ([]storedSpoolRecord, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("transport: open spool: %w", err)
	}
	defer f.Close()
	var records []storedSpoolRecord
	var total int64
	for offset := int64(0); ; {
		var length uint32
		err := binary.Read(f, binary.BigEndian, &length)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || length == 0 || int64(length) > maxSpoolRecordBytes {
			return nil, fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, offset)
		}
		if total+int64(4+length) > s.maxSize {
			return nil, ErrSpoolRecordTooBig
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(f, payload); err != nil {
			return nil, fmt.Errorf("%w at offset %d: %v", ErrSpoolCorrupt, offset, err)
		}
		var record storedSpoolRecord
		if err := json.Unmarshal(payload, &record); err != nil || validateSpoolBatch(record.Batch) != nil {
			return nil, fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, offset)
		}
		records = append(records, record)
		total += int64(4 + length)
		offset = total
	}
	return records, nil
}

func (s *Spool) write(records []storedSpoolRecord) error {
	if spoolSize(records) > s.maxSize {
		return ErrSpoolRecordTooBig
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("transport: create spool directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".spool-*")
	if err != nil {
		return fmt.Errorf("transport: create spool temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("transport: restrict spool temporary file: %w", err)
	}
	for _, record := range records {
		encoded, err := encodeSpool(record)
		if err != nil {
			_ = tmp.Close()
			return err
		}
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(encoded)))
		if _, err := tmp.Write(length[:]); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("transport: write spool length: %w", err)
		}
		if _, err := tmp.Write(encoded); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("transport: write spool record: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("transport: sync spool: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("transport: close spool: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("transport: replace spool: %w", err)
	}
	return os.Chmod(s.path, 0o600)
}

func encodeSpool(record storedSpoolRecord) ([]byte, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("transport: encode spool record: %w", err)
	}
	if len(payload) == 0 || int64(len(payload)) > maxSpoolRecordBytes {
		return nil, ErrSpoolRecordTooBig
	}
	return payload, nil
}

func spoolSize(records []storedSpoolRecord) int64 {
	var total int64
	for _, record := range records {
		encoded, err := encodeSpool(record)
		if err != nil {
			return maxSpoolRecordBytes + 1
		}
		total += int64(4 + len(encoded))
		if total > maxSpoolRecordBytes {
			return total
		}
	}
	return total
}

func evictionGaps(records []storedSpoolRecord) []contracts.CoverageGap {
	type interval struct{ from, to uint64 }
	ranges := make(map[contracts.CollectorEpoch][]interval)
	for _, record := range records {
		epoch := batchEpoch(record.Batch)
		if epoch == "" {
			continue
		}
		for _, sample := range record.Batch.Samples {
			// Coverage gaps are half-open. The maximum uint64 sequence has no
			// representable exclusive upper bound, so it cannot be reported as
			// an eviction interval; retaining it for retry is safer than lying
			// about complete coverage.
			if sample.Sequence < ^uint64(0) {
				ranges[epoch] = append(ranges[epoch], interval{from: sample.Sequence, to: sample.Sequence + 1})
			}
		}
		for _, gap := range record.Batch.Gaps {
			ranges[epoch] = append(ranges[epoch], interval{from: gap.FromSequence, to: gap.ToSequence})
		}
	}
	out := make([]contracts.CoverageGap, 0, len(ranges))
	for epoch, intervals := range ranges {
		sort.Slice(intervals, func(i, j int) bool {
			if intervals[i].from != intervals[j].from {
				return intervals[i].from < intervals[j].from
			}
			return intervals[i].to < intervals[j].to
		})
		for _, current := range intervals {
			if current.from >= current.to {
				continue
			}
			if len(out) > 0 {
				last := &out[len(out)-1]
				if last.CollectorEpoch == epoch && current.from <= last.ToSequence {
					if current.to > last.ToSequence {
						last.ToSequence = current.to
					}
					continue
				}
			}
			out = append(out, contracts.CoverageGap{CollectorEpoch: epoch, FromSequence: current.from, ToSequence: current.to, Reason: "spool-eviction"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CollectorEpoch < out[j].CollectorEpoch })
	return out
}

// evictionGapsFromGaps canonicalizes already-derived ranges without routing
// them through a mixed-epoch SampleBatch. Each durable record must contain one
// collector epoch because acknowledgement has no epoch field of its own.
func evictionGapsFromGaps(gaps []contracts.CoverageGap) []contracts.CoverageGap {
	records := make([]storedSpoolRecord, 0, len(gaps))
	for _, gap := range gaps {
		records = append(records, storedSpoolRecord{Batch: contracts.SampleBatch{Gaps: []contracts.CoverageGap{gap}}})
	}
	return evictionGaps(records)
}

func durableGapRecords(gaps []contracts.CoverageGap) []storedSpoolRecord {
	byEpoch := make(map[contracts.CollectorEpoch][]contracts.CoverageGap, len(gaps))
	for _, gap := range gaps {
		byEpoch[gap.CollectorEpoch] = append(byEpoch[gap.CollectorEpoch], gap)
	}
	epochs := make([]string, 0, len(byEpoch))
	for epoch := range byEpoch {
		epochs = append(epochs, string(epoch))
	}
	sort.Strings(epochs)
	records := make([]storedSpoolRecord, 0, len(epochs))
	for _, epoch := range epochs {
		records = append(records, storedSpoolRecord{Batch: contracts.SampleBatch{Gaps: byEpoch[contracts.CollectorEpoch(epoch)]}})
	}
	return records
}

func acknowledgeBatch(batch contracts.SampleBatch, epoch contracts.CollectorEpoch, through uint64) (contracts.SampleBatch, bool) {
	changed := false
	out := contracts.SampleBatch{Samples: make([]contracts.NodeMetricSample, 0, len(batch.Samples)), Gaps: make([]contracts.CoverageGap, 0, len(batch.Gaps))}
	for _, sample := range batch.Samples {
		if sample.CollectorEpoch == epoch && sample.Sequence <= through {
			changed = true
			continue
		}
		out.Samples = append(out.Samples, sample)
	}
	for _, gap := range batch.Gaps {
		if gap.CollectorEpoch != epoch || gap.FromSequence > through {
			out.Gaps = append(out.Gaps, gap)
			continue
		}
		changed = true
		if through < gap.ToSequence {
			gap.FromSequence = through + 1
			out.Gaps = append(out.Gaps, gap)
		}
	}
	return out, changed
}

func cloneBatch(batch contracts.SampleBatch) contracts.SampleBatch {
	clone := contracts.SampleBatch{Samples: append([]contracts.NodeMetricSample(nil), batch.Samples...), Gaps: append([]contracts.CoverageGap(nil), batch.Gaps...)}
	for i := range clone.Samples {
		clone.Samples[i].Values = cloneMap(clone.Samples[i].Values)
		clone.Samples[i].Counters = cloneMap(clone.Samples[i].Counters)
		clone.Samples[i].Units = cloneMap(clone.Samples[i].Units)
		clone.Samples[i].Validity = cloneMap(clone.Samples[i].Validity)
	}
	return clone
}

func cloneMap[T any](input map[string]T) map[string]T {
	if input == nil {
		return nil
	}
	output := make(map[string]T, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
