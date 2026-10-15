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
	"strconv"
	"strings"
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

// Spool is a bounded, crash-safe queue of length-prefixed JSON records.
//
// The common operations are cheap so a node stays light even after a long
// outage: Append writes one record at the end of the file, and acknowledging
// the oldest records only moves a head offset kept in a small side file.
// The file is rewritten (through a temporary file and rename) only when the
// bound forces eviction, when most of it has been acknowledged, or when an
// acknowledgement is not a clean prefix.
type Spool struct {
	path    string
	maxSize int64
	mu      sync.Mutex

	entries []spoolEntry // records from the start of the file
	head    int          // index of the first unacknowledged entry
	size    int64        // file length
}

type spoolEntry struct {
	offset int64
	length int64 // including the 4-byte prefix
	epoch  contracts.CollectorEpoch
	// low is the smallest sequence the record covers (samples and gap
	// starts); maxSample and maxGapTo bound what an acknowledgement must
	// reach to remove the record entirely.
	low        uint64
	hasSamples bool
	maxSample  uint64
	hasGaps    bool
	maxGapTo   uint64
}

// spoolEvictTarget is the share of the bound kept after an eviction, so the
// next appends are cheap again instead of evicting one record each time.
const spoolEvictTarget = 9

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
		if err := s.load(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("transport: inspect spool: %w", err)
	}
	return s, nil
}

// Path returns the configured queue path for diagnostics.
func (s *Spool) Path() string { return s.path }

func (s *Spool) headPath() string { return s.path + ".head" }

func (s *Spool) liveSize() int64 {
	if s.head >= len(s.entries) {
		return 0
	}
	return s.size - s.entries[s.head].offset
}

// load indexes the file. A record cut off by a crash during an append is
// trimmed; a complete record that does not decode is corruption.
func (s *Spool) load() error {
	f, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("transport: open spool: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("transport: inspect spool: %w", err)
	}
	fileSize := info.Size()
	entries := []spoolEntry{}
	offset := int64(0)
	for offset < fileSize {
		var prefix [4]byte
		if fileSize-offset < 4 {
			break // torn length prefix
		}
		if _, err := f.ReadAt(prefix[:], offset); err != nil {
			return fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, offset)
		}
		length := int64(binary.BigEndian.Uint32(prefix[:]))
		if length == 0 || length > maxSpoolRecordBytes {
			return fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, offset)
		}
		if offset+4+length > fileSize {
			break // torn record
		}
		payload := make([]byte, length)
		if _, err := f.ReadAt(payload, offset+4); err != nil {
			return fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, offset)
		}
		var record storedSpoolRecord
		if err := json.Unmarshal(payload, &record); err != nil || validateSpoolBatch(record.Batch) != nil {
			return fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, offset)
		}
		entries = append(entries, describeSpoolRecord(record, offset, 4+length))
		offset += 4 + length
	}
	if offset < fileSize {
		if err := os.Truncate(s.path, offset); err != nil {
			return fmt.Errorf("transport: trim torn spool record: %w", err)
		}
	}
	s.entries, s.size, s.head = entries, offset, 0
	if raw, err := os.ReadFile(s.headPath()); err == nil {
		if value, parseErr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); parseErr == nil {
			for index, entry := range entries {
				if entry.offset == value {
					s.head = index
					break
				}
			}
			if value == offset {
				s.head = len(entries)
			}
		}
	}
	if s.liveSize() > s.maxSize {
		// Written under a larger bound: evict now through the normal path.
		records, err := s.liveRecords()
		if err != nil {
			return err
		}
		records, _, err = s.evict(records)
		if err != nil {
			return err
		}
		return s.rewrite(records)
	}
	return nil
}

func describeSpoolRecord(record storedSpoolRecord, offset, length int64) spoolEntry {
	entry := spoolEntry{offset: offset, length: length, epoch: batchEpoch(record.Batch), low: ^uint64(0)}
	for _, sample := range record.Batch.Samples {
		entry.hasSamples = true
		entry.low = min(entry.low, sample.Sequence)
		entry.maxSample = max(entry.maxSample, sample.Sequence)
	}
	for _, gap := range record.Batch.Gaps {
		entry.hasGaps = true
		entry.low = min(entry.low, gap.FromSequence)
		entry.maxGapTo = max(entry.maxGapTo, gap.ToSequence)
	}
	return entry
}

// Append durably queues a batch. If older records must be evicted to remain
// within the byte bound, an explicit coverage-gap record is written before
// the remaining samples in the same atomic rewrite. The returned gaps are
// retained for diagnostics/backward-compatible callers; replay need not keep
// them in process memory.
func (s *Spool) Append(batch contracts.SampleBatch) ([]contracts.CoverageGap, error) {
	if err := validateSpoolBatch(batch); err != nil {
		return nil, err
	}
	record := storedSpoolRecord{Batch: batch}
	encoded, err := encodeSpool(record)
	if err != nil {
		return nil, err
	}
	length := int64(4 + len(encoded))
	if length > s.maxSize {
		return nil, ErrSpoolRecordTooBig
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The whole file (acknowledged prefix included) stays within the bound,
	// because older agents reject a larger file; drop the prefix first.
	if s.size+length > s.maxSize && s.head > 0 {
		if err := s.compact(); err != nil {
			return nil, err
		}
	}
	if s.size+length <= s.maxSize {
		if err := s.appendRaw(encoded); err != nil {
			return nil, err
		}
		s.entries = append(s.entries, describeSpoolRecord(record, s.size, length))
		s.size += length
		return nil, nil
	}
	return s.evictAndAppend(record, length)
}

// evictAndAppend makes room by dropping the oldest records down to
// spoolEvictTarget tenths of the bound. Only the evicted records are decoded
// (to describe their coverage gap); the kept records are copied as raw bytes,
// so a full spool never has to be held in memory.
func (s *Spool) evictAndAppend(record storedSpoolRecord, length int64) ([]contracts.CoverageGap, error) {
	live := s.entries[min(s.head, len(s.entries)):]
	target := s.maxSize * spoolEvictTarget / 10
	remaining := s.liveSize() + length
	cut := 0
	for cut < len(live) && remaining > target {
		remaining -= live[cut].length
		cut++
	}
	evicted, err := s.readEntries(live[:cut])
	if err != nil {
		return nil, err
	}
	gaps := evictionGaps(evicted)
	prefix := durableGapRecords(gaps)
	var prefixSize int64
	for _, gapRecord := range prefix {
		encoded, err := encodeSpool(gapRecord)
		if err != nil {
			return nil, err
		}
		prefixSize += int64(4 + len(encoded))
	}
	if prefixSize+remaining > s.maxSize {
		// Pathological gap records: fall back to the full decode path.
		records, err := s.liveRecords()
		if err != nil {
			return nil, err
		}
		records, gaps, err = s.evict(append(records, record))
		if err != nil {
			return nil, err
		}
		return gaps, s.rewrite(records)
	}
	if err := s.spliceRewrite(prefix, cut, &record); err != nil {
		return nil, err
	}
	return gaps, nil
}

// spliceRewrite writes prefix records, then the raw bytes of live entries
// from index skip onward, then an optional new record, and re-indexes without
// decoding the copied bytes.
func (s *Spool) spliceRewrite(prefix []storedSpoolRecord, skip int, suffix *storedSpoolRecord) error {
	live := s.entries[min(s.head, len(s.entries)):]
	kept := live[min(skip, len(live)):]
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("transport: create spool directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".spool-*")
	if err != nil {
		return fmt.Errorf("transport: create spool temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	entries := make([]spoolEntry, 0, len(prefix)+len(kept)+1)
	var offset int64
	writeRecord := func(record storedSpoolRecord) error {
		encoded, err := encodeSpool(record)
		if err != nil {
			return err
		}
		buf := make([]byte, 4+len(encoded))
		binary.BigEndian.PutUint32(buf, uint32(len(encoded)))
		copy(buf[4:], encoded)
		if _, err := tmp.Write(buf); err != nil {
			return fmt.Errorf("transport: write spool record: %w", err)
		}
		entries = append(entries, describeSpoolRecord(record, offset, int64(len(buf))))
		offset += int64(len(buf))
		return nil
	}
	fail := func(err error) error { _ = tmp.Close(); return err }
	for _, record := range prefix {
		if err := writeRecord(record); err != nil {
			return fail(err)
		}
	}
	if len(kept) > 0 {
		src, err := os.Open(s.path)
		if err != nil {
			return fail(fmt.Errorf("transport: open spool: %w", err))
		}
		start := kept[0].offset
		copied, err := io.Copy(tmp, io.NewSectionReader(src, start, s.size-start))
		_ = src.Close()
		if err != nil || copied != s.size-start {
			return fail(fmt.Errorf("transport: copy spool records: %v", err))
		}
		for _, entry := range kept {
			entry.offset = entry.offset - start + offset
			entries = append(entries, entry)
		}
		offset += copied
	}
	if suffix != nil {
		if err := writeRecord(*suffix); err != nil {
			return fail(err)
		}
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("transport: sync spool: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("transport: close spool: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("transport: replace spool: %w", err)
	}
	_ = os.Remove(s.headPath())
	s.entries, s.head, s.size = entries, 0, offset
	return nil
}

func (s *Spool) appendRaw(encoded []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("transport: create spool directory: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("transport: open spool: %w", err)
	}
	buf := make([]byte, 4+len(encoded))
	binary.BigEndian.PutUint32(buf, uint32(len(encoded)))
	copy(buf[4:], encoded)
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Truncate(s.path, s.size)
		return fmt.Errorf("transport: write spool record: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("transport: sync spool: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("transport: close spool: %w", err)
	}
	return os.Chmod(s.path, 0o600)
}

// evict drops the oldest records until the queue fits within
// spoolEvictTarget tenths of the bound, replacing them with durable
// coverage-gap records at the front.
func (s *Spool) evict(records []storedSpoolRecord) ([]storedSpoolRecord, []contracts.CoverageGap, error) {
	sizes := make([]int64, len(records))
	var total int64
	for i, record := range records {
		encoded, err := encodeSpool(record)
		if err != nil {
			return nil, nil, err
		}
		sizes[i] = int64(4 + len(encoded))
		total += sizes[i]
	}
	if total <= s.maxSize {
		return records, nil, nil
	}
	target := s.maxSize * spoolEvictTarget / 10
	cut := 0
	for total > target && cut < len(records)-1 {
		total -= sizes[cut]
		cut++
	}
	evicted, tail := records[:cut], records[cut:]
	gaps := evictionGaps(evicted)
	if len(gaps) == 0 {
		return tail, nil, nil
	}
	// The gap record is deliberately kept at the front. If adding it puts
	// the queue over the bound, evict the oldest sample after the gap and
	// fold its coverage into the same durable record.
	result := append(durableGapRecords(gaps), tail...)
	for spoolSize(result) > s.maxSize && len(tail) > 0 {
		victim := tail[0]
		tail = tail[1:]
		gaps = evictionGapsFromGaps(append(gaps, evictionGaps([]storedSpoolRecord{victim})...))
		result = append(durableGapRecords(gaps), tail...)
	}
	if spoolSize(result) > s.maxSize {
		return nil, nil, ErrSpoolRecordTooBig
	}
	return result, gaps, nil
}

// Pending returns up to limit unacknowledged records (all when limit <= 0),
// reading only those records from disk.
func (s *Spool) Pending(limit int) ([]SpoolRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.entries[min(s.head, len(s.entries)):]
	if limit <= 0 || limit > len(live) {
		limit = len(live)
	}
	records, err := s.readEntries(live[:limit])
	if err != nil {
		return nil, err
	}
	out := make([]SpoolRecord, 0, len(records))
	for _, record := range records {
		out = append(out, SpoolRecord{Batch: record.Batch})
	}
	return out, nil
}

func (s *Spool) liveRecords() ([]storedSpoolRecord, error) {
	return s.readEntries(s.entries[min(s.head, len(s.entries)):])
}

func (s *Spool) readEntries(entries []spoolEntry) ([]storedSpoolRecord, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("transport: open spool: %w", err)
	}
	defer f.Close()
	records := make([]storedSpoolRecord, 0, len(entries))
	for _, entry := range entries {
		payload := make([]byte, entry.length-4)
		if _, err := f.ReadAt(payload, entry.offset+4); err != nil {
			return nil, fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, entry.offset)
		}
		var record storedSpoolRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, fmt.Errorf("%w at offset %d", ErrSpoolCorrupt, entry.offset)
		}
		records = append(records, record)
	}
	return records, nil
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
	head := min(s.head, len(s.entries))
	for head < len(s.entries) && fullyAcknowledged(s.entries[head], epoch, through) {
		head++
	}
	clean := true
	for _, entry := range s.entries[head:] {
		if entry.epoch == epoch && entry.low <= through {
			clean = false
			break
		}
	}
	if !clean {
		// Not a plain prefix (a partially covered record): rewrite.
		s.head = head
		records, err := s.liveRecords()
		if err != nil {
			return err
		}
		kept := make([]storedSpoolRecord, 0, len(records))
		for _, record := range records {
			batch, _ := acknowledgeBatch(record.Batch, epoch, through)
			if len(batch.Samples) != 0 || len(batch.Gaps) != 0 {
				kept = append(kept, storedSpoolRecord{Batch: batch})
			}
		}
		return s.rewrite(kept)
	}
	if head == s.head {
		return nil
	}
	s.head = head
	if s.head >= len(s.entries) {
		return s.reset()
	}
	// Compact once most of the file is acknowledged; otherwise just record
	// the new head (losing it in a crash only resends records the hub
	// already accepted).
	if s.entries[s.head].offset*2 > s.size {
		return s.compact()
	}
	return writeFileAtomic(s.headPath(), []byte(strconv.FormatInt(s.entries[s.head].offset, 10)), 0o600, false)
}

// fullyAcknowledged mirrors acknowledgeBatch: a sample goes when its
// sequence is at or before through, a gap only when through reaches its end.
func fullyAcknowledged(entry spoolEntry, epoch contracts.CollectorEpoch, through uint64) bool {
	if entry.epoch != epoch {
		return false
	}
	if entry.hasSamples && entry.maxSample > through {
		return false
	}
	if entry.hasGaps && through < entry.maxGapTo {
		return false
	}
	return true
}

// reset empties the queue.
func (s *Spool) reset() error {
	if err := os.Truncate(s.path, 0); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("transport: truncate spool: %w", err)
	}
	_ = os.Remove(s.headPath())
	s.entries, s.head, s.size = nil, 0, 0
	return nil
}

// compact copies the unacknowledged bytes to a new file without decoding.
func (s *Spool) compact() error {
	return s.spliceRewrite(nil, 0, nil)
}

// rewrite replaces the file with records and re-indexes it.
func (s *Spool) rewrite(records []storedSpoolRecord) error {
	if err := s.write(records); err != nil {
		return err
	}
	_ = os.Remove(s.headPath())
	entries := make([]spoolEntry, 0, len(records))
	offset := int64(0)
	for _, record := range records {
		encoded, err := encodeSpool(record)
		if err != nil {
			return err
		}
		length := int64(4 + len(encoded))
		entries = append(entries, describeSpoolRecord(record, offset, length))
		offset += length
	}
	s.entries, s.head, s.size = entries, 0, offset
	return nil
}

// read decodes every unacknowledged record (diagnostics and tests).
func (s *Spool) read() ([]storedSpoolRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveRecords()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode, sync bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".spool-head-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if sync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
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
