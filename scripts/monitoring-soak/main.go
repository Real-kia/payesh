// Command monitoring-soak runs a bounded local-ingestion and retention soak.
//
// It is intentionally a test/operations tool, not a production daemon. The
// default run is short enough for CI; operators can select a longer duration
// (including 24h) explicitly. All state is placed in a caller-selected DB or
// a private temporary directory and is removed on exit unless -keep is set.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	defaultDuration        = 20 * time.Second
	defaultInterval        = 250 * time.Millisecond
	defaultPruneInterval   = 2 * time.Second
	defaultRetentionAge    = 10 * time.Second
	maxDuration            = 7 * 24 * time.Hour
	maxInterval            = 24 * time.Hour
	maxBatchSize           = 200
	maxLatencyObservations = 2_000_000
)

type config struct {
	duration      time.Duration
	interval      time.Duration
	pruneInterval time.Duration
	retentionAge  time.Duration
	dbPath        string
	maxBytes      int64
	batchSize     int
	keep          bool
	quiet         bool
}

type report struct {
	Format        string    `json:"format"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	Duration      float64   `json:"duration_seconds"`
	Interval      float64   `json:"sample_interval_seconds"`
	PruneInterval float64   `json:"prune_interval_seconds"`
	RetentionAge  float64   `json:"retention_age_seconds"`
	BatchSize     int       `json:"batch_size"`
	MaxBytes      int64     `json:"max_bytes"`

	AttemptedSamples int64 `json:"attempted_samples"`
	InsertedSamples  int64 `json:"inserted_samples"`
	DuplicateSamples int64 `json:"duplicate_samples"`
	IngestErrors     int64 `json:"ingest_errors"`
	PruneRuns        int64 `json:"prune_runs"`
	PruneErrors      int64 `json:"prune_errors"`
	PruneDeleted     int64 `json:"prune_deleted_samples"`
	StorageErrors    int64 `json:"storage_errors"`
	StoragePressure  bool  `json:"storage_pressure"`
	CleanTeardown    bool  `json:"clean_teardown"`

	IngestSamplesPerSecond float64 `json:"ingest_samples_per_second"`
	IngestP50Milliseconds  float64 `json:"ingest_p50_ms"`
	IngestP95Milliseconds  float64 `json:"ingest_p95_ms"`
	IngestP99Milliseconds  float64 `json:"ingest_p99_ms"`
	IngestMaxMilliseconds  float64 `json:"ingest_max_ms"`

	DatabaseBytesStart int64 `json:"database_bytes_start"`
	DatabaseBytesEnd   int64 `json:"database_bytes_end"`
	DatabaseBytesPeak  int64 `json:"database_bytes_peak"`
	WALBytesStart      int64 `json:"wal_bytes_start"`
	WALBytesEnd        int64 `json:"wal_bytes_end"`
	WALBytesPeak       int64 `json:"wal_bytes_peak"`
	StorageBytesStart  int64 `json:"storage_bytes_start"`
	StorageBytesEnd    int64 `json:"storage_bytes_end"`
	StorageBytesPeak   int64 `json:"storage_bytes_peak"`
	RetentionObserved  bool  `json:"retention_observed"`
}

func main() {
	flags := flag.NewFlagSet("monitoring-soak", flag.ExitOnError)
	cfg := config{}
	flags.DurationVar(&cfg.duration, "duration", defaultDuration, "bounded run duration (default 20s; maximum 168h)")
	flags.DurationVar(&cfg.interval, "interval", defaultInterval, "time between generated samples")
	flags.DurationVar(&cfg.pruneInterval, "prune-interval", defaultPruneInterval, "time between bounded retention passes")
	flags.DurationVar(&cfg.retentionAge, "retention-age", defaultRetentionAge, "raw sample retention age used by this benchmark")
	flags.StringVar(&cfg.dbPath, "db", "", "SQLite path; empty creates a private temporary database")
	flags.Int64Var(&cfg.maxBytes, "max-bytes", 512<<20, "managed storage ceiling; 0 disables the ceiling")
	flags.IntVar(&cfg.batchSize, "batch-size", 1, "samples ingested per interval (1..200)")
	flags.BoolVar(&cfg.keep, "keep", false, "keep the temporary database and print its path")
	flags.BoolVar(&cfg.quiet, "quiet", false, "suppress progress lines; JSON report is still printed")
	flags.Parse(os.Args[1:])
	if flags.NArg() != 0 {
		fatal(errors.New("monitoring-soak does not accept positional arguments"))
	}
	if err := validateConfig(cfg); err != nil {
		fatal(err)
	}
	report, err := run(context.Background(), cfg, os.Stderr, os.Stderr)
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "monitoring-soak:", err)
	os.Exit(2)
}

func validateConfig(cfg config) error {
	if cfg.duration <= 0 || cfg.duration > maxDuration {
		return fmt.Errorf("duration must be >0 and <= %s", maxDuration)
	}
	if cfg.interval <= 0 || cfg.interval > maxInterval {
		return fmt.Errorf("interval must be >0 and <= %s", maxInterval)
	}
	if cfg.pruneInterval <= 0 || cfg.pruneInterval > maxInterval {
		return fmt.Errorf("prune-interval must be >0 and <= %s", maxInterval)
	}
	if cfg.retentionAge <= 0 || cfg.retentionAge > maxDuration {
		return fmt.Errorf("retention-age must be >0 and <= %s", maxDuration)
	}
	if cfg.batchSize < 1 || cfg.batchSize > maxBatchSize {
		return fmt.Errorf("batch-size must be between 1 and %d", maxBatchSize)
	}
	if cfg.maxBytes < 0 {
		return errors.New("max-bytes must be >= 0")
	}
	if cfg.dbPath != "" && cfg.dbPath == ":memory:" {
		return errors.New("-db=:memory: is not supported because storage and WAL metrics require files")
	}
	return nil
}

func run(parent context.Context, cfg config, progress, diagnostics io.Writer) (report, error) {
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, cfg.duration+maxDuration) // cleanup gets a separate short context below.
	defer cancel()
	workDir := ""
	cleaned := false
	dbPath := cfg.dbPath
	if dbPath == "" {
		var err error
		workDir, err = os.MkdirTemp("", "payesh-monitoring-soak-")
		if err != nil {
			return report{}, fmt.Errorf("create private work directory: %w", err)
		}
		dbPath = filepath.Join(workDir, "payesh.db")
		if err := os.Mkdir(filepath.Join(workDir, "logs"), 0o700); err != nil {
			_ = os.RemoveAll(workDir)
			return report{}, fmt.Errorf("create managed log directory: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		if workDir != "" {
			_ = os.RemoveAll(workDir)
		}
		return report{}, fmt.Errorf("create database directory: %w", err)
	}
	managedRoot := filepath.Join(filepath.Dir(dbPath), "logs")
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		if workDir != "" {
			_ = os.RemoveAll(workDir)
		}
		return report{}, fmt.Errorf("create managed log directory: %w", err)
	}
	store, err := monitoring.OpenStore(ctx, dbPath, monitoring.StoreOptions{MaxBytes: cfg.maxBytes, ManagedPaths: []string{managedRoot}})
	if err != nil {
		if workDir != "" {
			_ = os.RemoveAll(workDir)
		}
		return report{}, fmt.Errorf("open store: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
		if workDir != "" && !cfg.keep && !cleaned {
			_ = os.RemoveAll(workDir)
		}
	}()

	serverID := contracts.ServerID("soak-server-00000001")
	if err := store.EnsureServer(ctx, contracts.Server{ID: serverID, Name: "Monitoring soak", Role: "standalone", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}, Version: "soak", ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		return report{}, fmt.Errorf("ensure benchmark server: %w", err)
	}
	// Raw ingestion is independent from derived processing. Acknowledge the
	// bounded post-process queue synchronously so it cannot become the benchmark
	// bottleneck or turn a long run into an artificial queue-capacity test.
	store.SetIngestionObserver(func(observerCtx context.Context, samples []contracts.MetricSample) error {
		for _, sample := range samples {
			if err := store.AcknowledgePostProcessSample(observerCtx, sample); err != nil {
				return err
			}
		}
		return nil
	})

	r := report{Format: "payesh.monitoring-soak.v1", StartedAt: started, Interval: cfg.interval.Seconds(), PruneInterval: cfg.pruneInterval.Seconds(), RetentionAge: cfg.retentionAge.Seconds(), BatchSize: cfg.batchSize, MaxBytes: cfg.maxBytes}
	if db, wal, total, err := sizes(dbPath, store); err == nil {
		r.DatabaseBytesStart, r.WALBytesStart, r.StorageBytesStart = db, wal, total
	} else {
		r.StorageErrors++
		fmt.Fprintf(diagnostics, "monitoring-soak: initial storage measurement: %v\n", err)
	}

	latencies := make([]float64, 0, 1024)
	var sequence atomic.Uint64
	epoch := contracts.CollectorEpoch("soak-epoch-00000001")
	nextSample := time.Now()
	nextPrune := nextSample.Add(cfg.pruneInterval)
	end := nextSample.Add(cfg.duration)
	ticker := time.NewTicker(minDuration(cfg.interval, cfg.pruneInterval))
	defer ticker.Stop()
	for now := nextSample; now.Before(end); {
		select {
		case <-ctx.Done():
			return r, fmt.Errorf("soak context ended: %w", ctx.Err())
		case <-ticker.C:
		}
		now = time.Now()
		for now.After(nextSample) || now.Equal(nextSample) {
			if nextSample.After(end) {
				break
			}
			batch := make([]contracts.MetricSample, 0, cfg.batchSize)
			for i := 0; i < cfg.batchSize; i++ {
				seq := sequence.Add(1) - 1
				observed := nextSample.Add(time.Duration(i) * time.Nanosecond)
				batch = append(batch, contracts.MetricSample{ServerID: serverID, CollectorEpoch: epoch, Sequence: seq, ObservedAt: observed.UTC(), ReceivedAt: observed.UTC(), Values: map[string]float64{"cpu.utilization": float64(seq % 100), "memory.used_bytes": float64(1024 + seq%4096)}, Counters: map[string]string{"net.billing.rx_bytes": fmt.Sprintf("%d", seq*4096), "net.billing.tx_bytes": fmt.Sprintf("%d", seq*2048)}, Units: map[string]string{"cpu.utilization": "percent", "memory.used_bytes": "bytes", "net.billing.rx_bytes": "bytes", "net.billing.tx_bytes": "bytes"}, Validity: map[string]string{"cpu.utilization": "valid", "memory.used_bytes": "valid", "net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid"}})
			}
			begin := time.Now()
			result, ingestErr := store.IngestSamples(ctx, serverID, batch, nil)
			latency := time.Since(begin).Seconds() * 1000
			if len(latencies) < maxLatencyObservations {
				latencies = append(latencies, latency)
			}
			r.AttemptedSamples += int64(len(batch))
			if ingestErr != nil {
				r.IngestErrors++
				if errors.Is(ingestErr, monitoring.ErrStoragePressure) {
					r.StoragePressure = true
				}
				fmt.Fprintf(diagnostics, "monitoring-soak: ingest error at sequence %d: %v\n", sequence.Load(), ingestErr)
			} else {
				r.InsertedSamples += int64(result.Inserted)
				r.DuplicateSamples += int64(result.Duplicate)
			}
			if db, wal, total, sizeErr := sizes(dbPath, store); sizeErr != nil {
				r.StorageErrors++
			} else {
				r.DatabaseBytesPeak = maxInt64(r.DatabaseBytesPeak, db)
				r.WALBytesPeak = maxInt64(r.WALBytesPeak, wal)
				r.StorageBytesPeak = maxInt64(r.StorageBytesPeak, total)
			}
			nextSample = nextSample.Add(cfg.interval)
			if nextSample.After(now) {
				break
			}
		}
		if !now.Before(nextPrune) {
			stats, pruneErr := store.Prune(ctx, now.UTC(), monitoring.RetentionPolicy{FullResolutionAge: cfg.retentionAge, MinuteRollupAge: cfg.retentionAge, HourRollupAge: cfg.retentionAge, TrafficPeriodAge: cfg.retentionAge, LogAge: cfg.retentionAge, LogMaxBytes: 1 << 20, BatchSize: maxInt(cfg.batchSize, 100), MetadataAge: cfg.retentionAge, MetadataMaxRows: 10000})
			r.PruneRuns++
			if pruneErr != nil {
				r.PruneErrors++
				fmt.Fprintf(diagnostics, "monitoring-soak: prune error: %v\n", pruneErr)
			} else {
				r.PruneDeleted += stats.MetricSamples
				if stats.MetricSamples > 0 {
					r.RetentionObserved = true
				}
			}
			nextPrune = now.Add(cfg.pruneInterval)
		}
		if !cfg.quiet {
			fmt.Fprintf(progress, "monitoring-soak progress: elapsed=%s attempted=%d inserted=%d pruned=%d\n", time.Since(started).Round(time.Millisecond), r.AttemptedSamples, r.InsertedSamples, r.PruneDeleted)
		}
	}
	ended := time.Now().UTC()
	// One final prune ensures a short run that crossed the retention age records
	// the same evidence as a periodic run, while still remaining bounded.
	if stats, pruneErr := store.Prune(ctx, ended, monitoring.RetentionPolicy{FullResolutionAge: cfg.retentionAge, MinuteRollupAge: cfg.retentionAge, HourRollupAge: cfg.retentionAge, TrafficPeriodAge: cfg.retentionAge, LogAge: cfg.retentionAge, LogMaxBytes: 1 << 20, BatchSize: maxInt(cfg.batchSize, 100), MetadataAge: cfg.retentionAge, MetadataMaxRows: 10000}); pruneErr != nil {
		r.PruneErrors++
	} else {
		r.PruneRuns++
		r.PruneDeleted += stats.MetricSamples
		r.RetentionObserved = r.RetentionObserved || stats.MetricSamples > 0
	}
	if db, wal, total, err := sizes(dbPath, store); err == nil {
		r.DatabaseBytesEnd, r.WALBytesEnd, r.StorageBytesEnd = db, wal, total
	} else {
		r.StorageErrors++
	}
	if err := store.Close(); err != nil {
		return r, fmt.Errorf("close store: %w", err)
	}
	closed = true
	if cfg.keep {
		retainedPath := dbPath
		if workDir != "" {
			retainedPath = workDir
		}
		fmt.Fprintf(progress, "monitoring-soak database retained at %s\n", retainedPath)
	}
	if workDir == "" || cfg.keep {
		r.CleanTeardown = workDir == "" && !cfg.keep
	} else {
		if err := os.RemoveAll(workDir); err != nil {
			return r, fmt.Errorf("remove temporary soak state: %w", err)
		}
		cleaned = true
		r.CleanTeardown = true
	}
	r.EndedAt = ended
	r.Duration = ended.Sub(started).Seconds()
	if r.Duration > 0 {
		r.IngestSamplesPerSecond = float64(r.InsertedSamples) / r.Duration
	}
	summarizeLatencies(&r, latencies)
	return r, nil
}

func sizes(dbPath string, store *monitoring.Store) (int64, int64, int64, error) {
	total, err := store.StorageBytes()
	if err != nil {
		return 0, 0, 0, err
	}
	db, err := fileSize(dbPath)
	if err != nil {
		return 0, 0, 0, err
	}
	wal, err := fileSize(dbPath + "-wal")
	if err != nil {
		return 0, 0, 0, err
	}
	return db, wal, total, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	return info.Size(), nil
}

func summarizeLatencies(r *report, values []float64) {
	if len(values) == 0 {
		return
	}
	sort.Float64s(values)
	percentile := func(p float64) float64 {
		index := int(math.Ceil(p*float64(len(values)))) - 1
		if index < 0 {
			index = 0
		}
		if index >= len(values) {
			index = len(values) - 1
		}
		return values[index]
	}
	r.IngestP50Milliseconds = percentile(.50)
	r.IngestP95Milliseconds = percentile(.95)
	r.IngestP99Milliseconds = percentile(.99)
	r.IngestMaxMilliseconds = values[len(values)-1]
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
