package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/collector"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/traffic"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "collect":
		err = collect(ctx, os.Args[2:])
	case "register-server":
		err = registerServer(ctx, os.Args[2:])
	case "ingest":
		err = ingest(ctx, os.Args[2:])
	case "metrics":
		err = queryMetrics(ctx, os.Args[2:])
	case "traffic":
		err = queryTraffic(ctx, os.Args[2:])
	case "read-log":
		err = readLog(ctx, os.Args[2:])
	case "capture-log":
		err = captureLog(ctx, os.Args[2:])
	case "logs":
		err = queryLogs(ctx, os.Args[2:])
	case "prune":
		err = prune(ctx, os.Args[2:])
	case "storage-usage":
		err = storageUsage(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "payesh:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: payesh <command> [options]

commands:
  collect          emit one node-owned metric sample as JSON
  register-server  create/update a local server record
	ingest           read node samples from stdin and durably ingest them (use --follow for a live stream)
  metrics          query bounded raw samples or minute/hour rollups
  traffic          query persisted allowance/usage periods
	read-log         read a configured regular file with bounds/redaction
	capture-log      read and persist a configured regular file snapshot
  logs             query persisted bounded log entries
  prune            remove expired history in bounded batches
  storage-usage    print database/WAL usage`)
}

func localStoreOptions() monitoring.StoreOptions {
	return monitoring.StoreOptions{MaxBytes: 512 << 20, ManagedPaths: []string{"/var/log/payesh"}}
}

func collect(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("collect", flag.ContinueOnError)
	root := flags.String("root", "/", "filesystem root containing proc-like host files")
	serverID := flags.String("server-id", string(monitoring.DefaultServerID), "stable server id")
	epoch := flags.String("epoch", "", "collector epoch override; omitted values get a fresh epoch per start")
	billingInterfaces := flags.String("billing-interfaces", "", "comma-separated authoritative billing interfaces")
	if err := flags.Parse(args); err != nil {
		return err
	}
	metricCollector := monitoring.NewCollector(*root, contracts.ServerID(*serverID), contracts.CollectorEpoch(*epoch))
	metricCollector.AuthoritativeInterfaces = splitCSV(*billingInterfaces)
	sample, err := metricCollector.Collect(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, sample)
}

func registerServer(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("register-server", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	id := flags.String("id", "", "stable server id; omitted values use the persisted installation identity")
	identityFile := flags.String("identity-file", "/var/lib/payesh/server-id", "persisted installation identity path")
	ensure := flags.Bool("ensure", false, "create the server record if absent without overwriting existing state")
	name := flags.String("name", "Local server", "display name")
	role := flags.String("role", "standalone", "server role")
	architecture := flags.String("architecture", runtime.GOARCH, "amd64 or arm64")
	platform := flags.String("platform", "linux", "platform label")
	capabilityCSV := flags.String("capabilities", "metrics,logs,traffic", "comma-separated capabilities")
	version := flags.String("version", "0.1.0", "agent version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resolvedID := contracts.ServerID(*id)
	if resolvedID == "" {
		var err error
		resolvedID, err = collector.LoadOrCreateServerID(*identityFile)
		if err != nil {
			return err
		}
	}
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	capabilities := make([]string, 0)
	for _, value := range strings.Split(*capabilityCSV, ",") {
		if value = strings.TrimSpace(value); value != "" {
			capabilities = append(capabilities, value)
		}
	}
	server := contracts.Server{ID: resolvedID, Name: *name, Role: *role, Architecture: *architecture, Platform: *platform, Capabilities: capabilities, Version: *version, ConnectionState: "never-connected", FreshnessState: "unknown"}
	if *ensure {
		if err := store.EnsureServer(ctx, server); err != nil {
			return err
		}
	} else if err := store.UpsertServer(ctx, server); err != nil {
		return err
	}
	return writeJSON(os.Stdout, server)
}

func ingest(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("ingest", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	serverID := flags.String("server-id", "", "server id for the batch; omitted values use the persisted installation identity")
	identityFile := flags.String("identity-file", "/var/lib/payesh/server-id", "persisted installation identity path")
	receivedAt := flags.String("received-at", "", "hub receipt RFC3339 timestamp; defaults to now")
	follow := flags.Bool("follow", false, "keep reading newline-delimited samples until the input closes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resolvedServerID := contracts.ServerID(*serverID)
	if resolvedServerID == "" {
		var err error
		resolvedServerID, err = collector.LoadOrCreateServerID(*identityFile)
		if err != nil {
			return err
		}
	}
	var receipt time.Time
	if *receivedAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, *receivedAt)
		if err != nil {
			return fmt.Errorf("received-at: %w", err)
		}
		receipt = parsed
	}
	explicitReceipt := !receipt.IsZero()
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	alertEngine, err := alerts.NewEngine(store)
	if err != nil {
		return err
	}
	if err := alertEngine.EnsureStarterRules(ctx, resolvedServerID); err != nil {
		return err
	}
	if destinations := alertDestinationsFromEnvironment(); len(destinations) > 0 {
		queue := alerts.NewDeliveryQueue(alerts.Notifier{}, 2)
		defer queue.Close()
		alertEngine.ConfigureDelivery(queue, func(context.Context, contracts.ServerID) []alerts.NotificationDestination {
			return append([]alerts.NotificationDestination(nil), destinations...)
		})
	}
	processor, err := traffic.NewProcessor(store, alertEngine)
	if err != nil {
		return err
	}
	store.SetIngestionObserver(processor.ObserveIngestion)
	var postProcessCancel context.CancelFunc
	var postProcessDone <-chan struct{}
	if *follow {
		// A follow process is long-lived, so keep retrying durable derived work
		// even when startup finds an out-of-order stream. The worker is bounded
		// per pass and serialized with post-commit callbacks by the processor.
		postProcessCtx, cancel := context.WithCancel(ctx)
		postProcessCancel = cancel
		postProcessDone = processor.StartPostProcessRetry(postProcessCtx, traffic.DefaultPostProcessRetryInterval, func(err error) {
			if postProcessCtx.Err() == nil {
				fmt.Fprintln(os.Stderr, "retry pending sample processing:", err)
			}
		})
		defer func() {
			postProcessCancel()
			<-postProcessDone
		}()
	} else if err := processor.DrainPendingPostProcess(ctx); err != nil {
		// A one-shot invocation has no worker to own a deferred queue. Surface
		// the explicit pending status instead of claiming startup drained it.
		return fmt.Errorf("drain pending sample processing: %w", err)
	}
	var heartbeatDone chan struct{}
	var heartbeatStop chan struct{}
	if *follow {
		heartbeatStop = make(chan struct{})
		heartbeatDone = make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatStop:
					return
				case now := <-ticker.C:
					// Heartbeats have an independent cadence from metric samples;
					// a one-hour economy interval must not make the server appear
					// healthy for an hour after the agent stops.
					_ = store.TouchServer(ctx, resolvedServerID, now.UTC())
				}
			}
		}()
		defer func() {
			close(heartbeatStop)
			<-heartbeatDone
		}()
	}
	decoder := json.NewDecoder(os.Stdin)
	var aggregate monitoring.IngestResult
	samplesRead := 0
	for {
		if !*follow && samplesRead == contracts.MaxBatchSamples {
			var extra json.RawMessage
			if err := decoder.Decode(&extra); err != io.EOF {
				if err == nil {
					return errors.New("stdin contains more than 200 samples")
				}
				return err
			}
			break
		}
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if len(raw) > contracts.MaxEnvelopeBytes {
			return errors.New("stdin sample exceeds envelope limit")
		}
		var sample contracts.NodeMetricSample
		if err := json.Unmarshal(raw, &sample); err != nil {
			return err
		}
		if !explicitReceipt {
			// Receipt time belongs to the hub, so capture it after this sample has
			// been decoded and the durable store is ready.
			receipt = time.Now().UTC()
		}
		result, err := store.IngestNodeBatch(ctx, resolvedServerID, []contracts.NodeMetricSample{sample}, nil, receipt)
		if err != nil {
			return err
		}
		// A successful local ingest is also the agent heartbeat. This update is
		// deliberately best-effort: the sample acknowledgement must not be turned
		// into a failure after its durable commit.
		_ = store.TouchServer(ctx, resolvedServerID, time.Now().UTC())
		samplesRead++
		aggregate.Inserted += result.Inserted
		aggregate.Duplicate += result.Duplicate
		aggregate.RollupsPending = aggregate.RollupsPending || result.RollupsPending
		aggregate.PostProcessPending = aggregate.PostProcessPending || result.PostProcessPending
		if *follow {
			if err := writeJSON(os.Stdout, result); err != nil {
				return err
			}
		}
		if !explicitReceipt {
			receipt = time.Time{}
		}
	}
	if samplesRead == 0 {
		return errors.New("stdin did not contain a node metric sample")
	}
	if *follow {
		return nil
	}
	return writeJSON(os.Stdout, aggregate)
}

func queryMetrics(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("metrics", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	serverID := flags.String("server-id", string(monitoring.DefaultServerID), "server id")
	fromValue := flags.String("from", "", "RFC3339 lower bound")
	toValue := flags.String("to", "", "RFC3339 upper bound")
	resolution := flags.String("resolution", "raw", "raw, minute, or hour")
	limit := flags.Int("limit", monitoring.MaxPageItems, "maximum result count (1..200)")
	cursor := flags.String("cursor", "", "opaque cursor")
	gapsCursor := flags.String("gaps-cursor", "", "opaque coverage-gap cursor")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *limit < 1 || *limit > monitoring.MaxPageItems {
		return errors.New("--limit must be 1..200")
	}
	from, to, err := parseBounds(*fromValue, *toValue)
	if err != nil {
		return err
	}
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	if *resolution == "minute" || *resolution == "hour" {
		seconds := 60
		if *resolution == "hour" {
			seconds = 3600
		}
		rollups, nextCursor, truncated, err := store.QueryRollups(ctx, contracts.ServerID(*serverID), from, to, seconds, *limit, *cursor)
		if err != nil {
			return err
		}
		page := monitoring.MetricPage{Samples: make([]contracts.MetricSample, 0), Rollups: rollups, Coverage: monitoring.CoverageForSamples(nil, rollups), NextCursor: nextCursor, Truncated: truncated}
		return writeJSON(os.Stdout, page)
	} else if *resolution != "raw" {
		return errors.New("resolution must be raw, minute, or hour")
	}
	page, err := store.QueryMetricsWithGapCursor(ctx, contracts.ServerID(*serverID), from, to, *limit, *cursor, *gapsCursor)
	if err != nil {
		return err
	}
	if page.Samples == nil {
		page.Samples = make([]contracts.MetricSample, 0)
	}
	if page.Rollups == nil {
		page.Rollups = make([]monitoring.Rollup, 0)
	}
	page.Coverage = monitoring.CoverageForSamples(page.Samples, page.Rollups, page.Gaps)
	return writeJSON(os.Stdout, page)
}

func queryTraffic(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("traffic", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	serverID := flags.String("server-id", string(monitoring.DefaultServerID), "server id")
	fromValue := flags.String("from", "", "RFC3339 lower bound")
	toValue := flags.String("to", "", "RFC3339 upper bound")
	scope := flags.String("scope", "", "optional traffic scope")
	limit := flags.Int("limit", monitoring.MaxPageItems, "maximum result count (1..200)")
	cursor := flags.String("cursor", "", "opaque cursor")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *limit < 1 || *limit > monitoring.MaxPageItems {
		return errors.New("--limit must be 1..200")
	}
	from, to, err := parseBounds(*fromValue, *toValue)
	if err != nil {
		return err
	}
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	page, err := store.QueryTrafficPeriods(ctx, contracts.ServerID(*serverID), from, to, *scope, *limit, *cursor)
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, page)
}

func readLog(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("read-log", flag.ContinueOnError)
	path := flags.String("path", "", "approved regular log file")
	serverID := flags.String("server-id", string(monitoring.DefaultServerID), "server id")
	sourceID := flags.String("source", "local-file", "stable source id")
	label := flags.String("label", "Local file", "source label")
	search := flags.String("search", "", "case-insensitive text filter")
	limit := flags.Int("limit", monitoring.DefaultLogEntries, "maximum entries")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--path is required")
	}
	result, err := monitoring.ReadConfiguredLog(ctx, monitoring.LogSource{ServerID: contracts.ServerID(*serverID), ID: *sourceID, Label: *label, Path: *path}, monitoring.LogReadOptions{MaxEntries: *limit, Search: *search})
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, result)
}

func captureLog(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("capture-log", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	serverID := flags.String("server-id", "", "server id; omitted values use the persisted installation identity")
	identityFile := flags.String("identity-file", "/var/lib/payesh/server-id", "persisted installation identity path")
	path := flags.String("path", "", "approved regular log file")
	sourceID := flags.String("source", "local-file", "stable source id")
	label := flags.String("label", "Local file", "source label")
	fromValue := flags.String("from", "", "RFC3339 lower bound")
	toValue := flags.String("to", "", "RFC3339 upper bound")
	severity := flags.String("severity", "", "exact severity filter")
	search := flags.String("search", "", "case-insensitive text filter")
	limit := flags.Int("limit", monitoring.DefaultLogEntries, "maximum entries")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || *sourceID == "" {
		return errors.New("--path and --source are required")
	}
	if *limit < 1 || *limit > monitoring.MaxPageItems {
		return errors.New("--limit must be 1..200")
	}
	from, to, err := parseOptionalBounds(*fromValue, *toValue)
	if err != nil {
		return err
	}
	resolvedID := contracts.ServerID(*serverID)
	if resolvedID == "" {
		resolvedID, err = collector.LoadOrCreateServerID(*identityFile)
		if err != nil {
			return err
		}
	}
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.RegisterLogSource(ctx, monitoring.LogSource{ServerID: resolvedID, ID: *sourceID, Label: *label, Path: *path}); err != nil {
		return err
	}
	result, err := monitoring.ReadConfiguredLog(ctx, monitoring.LogSource{ServerID: resolvedID, ID: *sourceID, Label: *label, Path: *path}, monitoring.LogReadOptions{MaxEntries: *limit, From: from, To: to, Severity: *severity, Search: *search})
	if err != nil {
		return err
	}
	inserted, err := store.InsertLogEntries(ctx, result.Entries)
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, map[string]any{"entries": result.Entries, "inserted": inserted, "truncated": result.Truncated})
}

func queryLogs(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	serverID := flags.String("server-id", string(monitoring.DefaultServerID), "server id")
	sourceID := flags.String("source", "", "source id")
	fromValue := flags.String("from", "", "RFC3339 lower bound")
	toValue := flags.String("to", "", "RFC3339 upper bound")
	severity := flags.String("severity", "", "exact severity filter")
	search := flags.String("search", "", "case-insensitive text filter")
	limit := flags.Int("limit", monitoring.MaxPageItems, "maximum result count")
	cursor := flags.String("cursor", "", "opaque cursor")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *limit < 1 || *limit > monitoring.MaxPageItems {
		return errors.New("--limit must be 1..200")
	}
	if *sourceID == "" {
		return errors.New("--source is required")
	}
	from, to, err := parseBounds(*fromValue, *toValue)
	if err != nil {
		return err
	}
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	source, found, err := store.GetLogSource(ctx, contracts.ServerID(*serverID), *sourceID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("configured log source was not found")
	}
	if source.Path != "" {
		decodedCursor := ""
		if *cursor != "" {
			decodedCursor, err = monitoring.DecodeLogCursor(*cursor)
			if err != nil {
				return err
			}
		}
		snapshot, readErr := monitoring.ReadConfiguredLogCursor(ctx, source, monitoring.LogReadOptions{MaxEntries: monitoring.MaxPageItems, MaxBytes: monitoring.MaxLogBytes, MaxLineSize: monitoring.DefaultLogLineSize, From: from, To: to, Severity: *severity, Search: *search}, decodedCursor)
		if readErr != nil {
			return readErr
		}
		if len(snapshot.Entries) > 0 {
			if _, err := store.InsertLogEntries(ctx, snapshot.Entries); err != nil {
				return err
			}
		}
	} else {
		journalCursor := ""
		if *cursor != "" {
			journalCursor, err = monitoring.DecodeLogCursor(*cursor)
			if err != nil {
				return err
			}
		}
		snapshot, readErr := monitoring.ReadJournal(ctx, source, monitoring.JournalOptions{Unit: source.ID, Cursor: journalCursor, MaxEntries: monitoring.MaxPageItems, MaxBytes: monitoring.MaxLogBytes, Since: from, Until: to})
		if readErr != nil {
			return readErr
		}
		if len(snapshot.Entries) > 0 {
			if _, err := store.InsertLogEntries(ctx, snapshot.Entries); err != nil {
				return err
			}
		}
	}
	page, err := store.QueryLogsFiltered(ctx, contracts.ServerID(*serverID), *sourceID, from, to, *severity, *search, *limit, *cursor)
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, page)
}

func prune(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("prune", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	stats, err := store.Prune(ctx, time.Now().UTC(), monitoring.DefaultRetentionPolicy())
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, stats)
}

func storageUsage(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("storage-usage", flag.ContinueOnError)
	dbPath := flags.String("db", "payesh.db", "SQLite database path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, err := monitoring.OpenStore(ctx, *dbPath, localStoreOptions())
	if err != nil {
		return err
	}
	defer store.Close()
	usage, err := store.StorageBytes()
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, map[string]int64{"bytes": usage})
}

func parseBounds(fromValue, toValue string) (time.Time, time.Time, error) {
	if fromValue == "" || toValue == "" {
		return time.Time{}, time.Time{}, errors.New("--from and --to are required")
	}
	from, err := time.Parse(time.RFC3339Nano, fromValue)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := time.Parse(time.RFC3339Nano, toValue)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to.Before(from) || to.Sub(from) > 31*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("time range must be ordered and no longer than 31 days")
	}
	return from, to, nil
}

func parseOptionalBounds(fromValue, toValue string) (time.Time, time.Time, error) {
	if fromValue == "" && toValue == "" {
		return time.Time{}, time.Time{}, nil
	}
	if fromValue == "" || toValue == "" {
		return time.Time{}, time.Time{}, errors.New("--from and --to must be supplied together")
	}
	return parseBounds(fromValue, toValue)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := make([]string, 0)
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func alertDestinationsFromEnvironment() []alerts.NotificationDestination {
	destinations := make([]alerts.NotificationDestination, 0, 2)
	if endpoint := strings.TrimSpace(os.Getenv("PAYESH_ALERT_WEBHOOK_URL")); endpoint != "" {
		destinations = append(destinations, alerts.NotificationDestination{Kind: "webhook", URL: endpoint, Secret: os.Getenv("PAYESH_ALERT_WEBHOOK_SECRET"), Enabled: true})
	}
	if token := os.Getenv("PAYESH_ALERT_TELEGRAM_TOKEN"); token != "" {
		apiURL := strings.TrimSpace(os.Getenv("PAYESH_ALERT_TELEGRAM_API_URL"))
		if apiURL == "" {
			apiURL = "https://api.telegram.org"
		}
		destinations = append(destinations, alerts.NotificationDestination{Kind: "telegram", URL: apiURL, Secret: token, ChatID: os.Getenv("PAYESH_ALERT_TELEGRAM_CHAT_ID"), Enabled: true})
	}
	return destinations
}
