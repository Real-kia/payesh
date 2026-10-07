package updater

// This file owns the data-transfer boundary used by role transitions. It is
// deliberately implemented against database/sql rather than monitoring.Store:
// exports/imports must be usable by the updater before the candidate server is
// running, and the source database remains untouched until verification.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

const (
	MigrationFormat                         = "payesh.migration.v1"
	MigrationSchemaVersion                  = 1
	MigrationReplayFormat                   = "payesh.migration.v2"
	MigrationReplaySchemaVersion            = 2
	ExportSingleServer           ExportKind = "single-server"
	ExportFullHub                ExportKind = "full-hub"
	MigrationKDF                            = "pbkdf2-hmac-sha256"
	MigrationKDFIterations                  = 200000
	MigrationMinKDFIterations               = 200000
	MigrationMaxKDFIterations               = 1000000
	maxMigrationBytes                       = 64 << 20
)

type ExportKind string

type ExportOptions struct {
	Kind       ExportKind
	ServerID   contracts.ServerID
	Passphrase string
	// From/To optionally restrict raw history. A caller doing a role cutover
	// normally leaves them empty so the complete retained range is transferred.
	From, To time.Time
	// ReplaySafe exports a complete schema-6 retained-state snapshot (v2).
	// Range-filtered exports remain v1 because replay authority is not a range.
	ReplaySafe bool
	serverIDs  []contracts.ServerID
}

type ImportOptions struct {
	Passphrase string
	// PreviousArtifact explicitly authorizes a v2 tail reconciliation only when
	// the destination still matches the exact previously imported scoped state.
	PreviousArtifact []byte
}

type MigrationExport struct {
	Format              string                    `json:"format"`
	SchemaVersion       int                       `json:"schema_version"`
	Kind                ExportKind                `json:"kind"`
	ExportID            string                    `json:"export_id"`
	CreatedAt           time.Time                 `json:"created_at"`
	SourceServerID      contracts.ServerID        `json:"source_server_id,omitempty"`
	Servers             []contracts.Server        `json:"servers"`
	Samples             []contracts.MetricSample  `json:"samples,omitempty"`
	Gaps                []ExportGap               `json:"gaps,omitempty"`
	Policies            []contracts.ControlPolicy `json:"policies,omitempty"`
	TrafficPeriods      []ExportTrafficPeriod     `json:"traffic_periods,omitempty"`
	Rollups             []ExportRollup            `json:"rollups,omitempty"`
	ReplayState         []ExportReplayTable       `json:"replay_state,omitempty"`
	RetirementSaturated *bool                     `json:"retirement_saturated,omitempty"`
	// Hub authority is included only in encrypted full-hub artifacts. These
	// fields are intentionally absent from single-server exports.
	BrowserAuthState   []byte `json:"browser_auth_state,omitempty"`
	FleetIdentityState []byte `json:"fleet_identity_state,omitempty"`
}

// ExportRollup mirrors monitoring.Rollup without making updater depend on the
// monitoring package. Rollups are optional derived data; raw samples and gaps
// remain the authority and can be rebuilt after import.
type ExportRollup struct {
	ServerID        contracts.ServerID `json:"server_id"`
	Metric          string             `json:"metric"`
	BucketStart     time.Time          `json:"bucket_start"`
	BucketSeconds   int                `json:"bucket_seconds"`
	SampleCount     int                `json:"sample_count"`
	ObservedSeconds float64            `json:"observed_seconds"`
	Minimum         *float64           `json:"minimum,omitempty"`
	Maximum         *float64           `json:"maximum,omitempty"`
	WeightedMean    *float64           `json:"weighted_mean,omitempty"`
	CounterDelta    string             `json:"counter_delta,omitempty"`
	Coverage        string             `json:"coverage"`
}

type ExportGap struct {
	ServerID contracts.ServerID    `json:"server_id"`
	Gap      contracts.CoverageGap `json:"gap"`
}

type ExportTrafficPeriod struct {
	ServerID contracts.ServerID      `json:"server_id"`
	Period   contracts.TrafficPeriod `json:"period"`
}

type migrationEnvelope struct {
	Format     string     `json:"format"`
	Kind       ExportKind `json:"kind"`
	Encrypted  bool       `json:"encrypted"`
	KDF        string     `json:"kdf,omitempty"`
	Iterations int        `json:"iterations,omitempty"`
	Salt       string     `json:"salt,omitempty"`
	Nonce      string     `json:"nonce,omitempty"`
	Payload    string     `json:"payload"`
}

type ImportResult struct {
	ExportID       string `json:"export_id"`
	Servers        int    `json:"servers"`
	Samples        int    `json:"samples"`
	Duplicate      int    `json:"duplicate"`
	Policies       int    `json:"policies"`
	TrafficPeriods int    `json:"traffic_periods"`
	Rollups        int    `json:"rollups"`
	AlreadyApplied bool   `json:"already_applied"`
}

var migrationServerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{15,127}$`)

// ExportMigration takes a read transaction, giving all rows one SQLite
// snapshot even when the source is in WAL mode. It never exports auth state,
// fleet identity state, job action payloads, enrollment authority, or private
// keys in a single-server artifact. V1 full-hub artifacts include encrypted
// browser/fleet identity state; opt-in v2 exports retained history and replay
// state only, excluding that execution authority even for full-hub exports.
// Full-hub artifacts of either version are always encrypted.
func ExportMigration(ctx context.Context, source *sql.DB, opts ExportOptions) ([]byte, error) {
	if opts.ReplaySafe && (!opts.From.IsZero() || !opts.To.IsZero()) {
		return nil, errors.New("updater: replay-safe migration requires complete retained history")
	}
	if source == nil {
		return nil, errors.New("updater: source database is required")
	}
	if opts.Kind == "" {
		opts.Kind = ExportSingleServer
	}
	if opts.Kind != ExportSingleServer && opts.Kind != ExportFullHub {
		return nil, errors.New("updater: invalid migration export kind")
	}
	if opts.Kind == ExportSingleServer && !validMigrationServerID(opts.ServerID) {
		return nil, errors.New("updater: single-server export requires a valid server id")
	}
	if !opts.From.IsZero() && !opts.To.IsZero() && opts.To.Before(opts.From) {
		return nil, errors.New("updater: migration history range is invalid")
	}
	if opts.Kind == ExportFullHub && len(opts.Passphrase) < 12 {
		return nil, errors.New("updater: full-hub export requires a passphrase of at least 12 characters")
	}
	tx, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("updater: begin migration snapshot: %w", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"servers", "metric_samples", "coverage_gaps"} {
		if ok, e := migrationTableExists(ctx, tx, table); e != nil {
			return nil, e
		} else if !ok {
			return nil, fmt.Errorf("updater: source schema is missing %s", table)
		}
	}

	exportID, err := randomExportID()
	if err != nil {
		return nil, err
	}
	export := MigrationExport{Format: MigrationFormat, SchemaVersion: MigrationSchemaVersion, Kind: opts.Kind, ExportID: exportID, CreatedAt: time.Now().UTC()}
	if opts.ReplaySafe {
		export.Format, export.SchemaVersion = MigrationReplayFormat, MigrationReplaySchemaVersion
	}
	if opts.Kind == ExportSingleServer {
		export.SourceServerID = opts.ServerID
	}
	if err := readMigrationRows(ctx, tx, &export, opts); err != nil {
		return nil, err
	}
	if opts.ReplaySafe {
		if err := validateMigrationExport(export); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("updater: commit migration snapshot: %w", err)
	}
	payload, err := json.Marshal(export)
	if err != nil {
		return nil, fmt.Errorf("updater: encode migration export: %w", err)
	}
	return wrapMigrationPayload(payload, export.Kind, opts.Passphrase)
}

func readMigrationRows(ctx context.Context, tx *sql.Tx, out *MigrationExport, opts ExportOptions) error {
	serverQuery := `SELECT id,name,role,architecture,platform,capabilities_json,version,last_heartbeat,connection_state,freshness_state,COALESCE(freshness_reason,''),configuration_revision FROM servers`
	args := []any{}
	if opts.Kind == ExportSingleServer {
		serverQuery += ` WHERE id=?`
		args = append(args, string(opts.ServerID))
	}
	if len(opts.serverIDs) > 0 {
		serverQuery = strings.Split(serverQuery, " WHERE ")[0] + " WHERE id IN (" + placeholders(len(opts.serverIDs)) + ")"
		args = nil
		for _, id := range opts.serverIDs {
			args = append(args, string(id))
		}
	}
	serverQuery += " ORDER BY id"
	rows, err := tx.QueryContext(ctx, serverQuery, args...)
	if err != nil {
		return fmt.Errorf("updater: read server records: %w", err)
	}
	for rows.Next() {
		server, err := scanMigrationServer(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if !validMigrationServerID(server.ID) {
			rows.Close()
			return errors.New("updater: source contains an invalid server id")
		}
		out.Servers = append(out.Servers, server)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if opts.Kind == ExportSingleServer && len(out.Servers) != 1 {
		return sql.ErrNoRows
	}
	serverIDs := make([]any, 0, len(out.Servers))
	for _, server := range out.Servers {
		serverIDs = append(serverIDs, string(server.ID))
	}
	if err := readMigrationSamples(ctx, tx, out, serverIDs, opts); err != nil {
		return err
	}
	if err := readMigrationGaps(ctx, tx, out, serverIDs); err != nil {
		return err
	}
	if err := readMigrationPolicies(ctx, tx, out, serverIDs); err != nil {
		return err
	}
	if err := readMigrationTraffic(ctx, tx, out, serverIDs); err != nil {
		return err
	}
	if opts.Kind == ExportFullHub && !opts.ReplaySafe {
		if err := readMigrationAuthority(ctx, tx, out); err != nil {
			return err
		}
	}
	if err := readMigrationRollups(ctx, tx, out, serverIDs); err != nil {
		return err
	}
	if opts.ReplaySafe {
		return readReplayState(ctx, tx, out, serverIDs)
	}
	return nil
}

func readMigrationAuthority(ctx context.Context, tx *sql.Tx, out *MigrationExport) error {
	for table, target := range map[string]*[]byte{"browser_auth_state": &out.BrowserAuthState, "fleet_identity_state": &out.FleetIdentityState} {
		ok, err := migrationTableExists(ctx, tx, table)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var state []byte
		if err := tx.QueryRowContext(ctx, `SELECT state_json FROM `+table+` WHERE singleton=1`).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return fmt.Errorf("updater: read %s: %w", table, err)
		}
		*target = append([]byte(nil), state...)
	}
	return nil
}

func readMigrationSamples(ctx context.Context, tx *sql.Tx, out *MigrationExport, ids []any, opts ExportOptions) error {
	if len(ids) == 0 {
		return nil
	}
	query := `SELECT server_id,collector_epoch,sequence,observed_at,received_at,COALESCE(timestamp_uncertainty,''),values_json,COALESCE(counters_json,'{}'),COALESCE(units_json,'{}'),COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id IN (` + placeholders(len(ids)) + `)`
	args := append([]any(nil), ids...)
	if !opts.From.IsZero() {
		query += ` AND observed_at >= ?`
		args = append(args, opts.From.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	}
	if !opts.To.IsZero() {
		query += ` AND observed_at <= ?`
		args = append(args, opts.To.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	}
	query += ` ORDER BY server_id,observed_at,length(sequence),sequence,collector_epoch`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("updater: read metric history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sample contracts.MetricSample
		var serverID, epoch, sequence, observed, received, uncertainty, values, counters, units, validity string
		if err := rows.Scan(&serverID, &epoch, &sequence, &observed, &received, &uncertainty, &values, &counters, &units, &validity); err != nil {
			return err
		}
		if err := parseMigrationSample(&sample, serverID, epoch, sequence, observed, received, uncertainty, values, counters, units, validity); err != nil {
			return err
		}
		out.Samples = append(out.Samples, sample)
	}
	return rows.Err()
}

func readMigrationGaps(ctx context.Context, tx *sql.Tx, out *MigrationExport, ids []any) error {
	if len(ids) == 0 {
		return nil
	}
	query := `SELECT server_id,collector_epoch,from_sequence,to_sequence,reason FROM coverage_gaps WHERE server_id IN (` + placeholders(len(ids)) + `) ORDER BY server_id,collector_epoch,length(from_sequence),from_sequence,length(to_sequence),to_sequence,reason`
	rows, err := tx.QueryContext(ctx, query, ids...)
	if err != nil {
		return fmt.Errorf("updater: read coverage history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var serverID, epoch, from, to, reason string
		if err := rows.Scan(&serverID, &epoch, &from, &to, &reason); err != nil {
			return err
		}
		fromN, err := strconv.ParseUint(from, 10, 64)
		if err != nil {
			return errors.New("updater: invalid source coverage range")
		}
		toN, err := strconv.ParseUint(to, 10, 64)
		if err != nil {
			return errors.New("updater: invalid source coverage range")
		}
		gap := contracts.CoverageGap{CollectorEpoch: contracts.CollectorEpoch(epoch), FromSequence: fromN, ToSequence: toN, Reason: reason}
		if err := gap.Validate(); err != nil {
			return fmt.Errorf("updater: invalid source coverage gap: %w", err)
		}
		// ServerID is not part of CoverageGap's wire shape; attach it through
		// the sample stream by using the per-export server order below.
		out.Gaps = append(out.Gaps, ExportGap{ServerID: contracts.ServerID(serverID), Gap: gap})
	}
	return rows.Err()
}

func readMigrationPolicies(ctx context.Context, tx *sql.Tx, out *MigrationExport, ids []any) error {
	if len(ids) == 0 {
		return nil
	}
	if ok, err := migrationTableExists(ctx, tx, "control_policies"); err != nil {
		return err
	} else if !ok {
		return nil
	}
	query := `SELECT server_id,module_id,target_kind,target_name,kind,state,parameters_json,revision,updated_at,error_json FROM control_policies WHERE server_id IN (` + placeholders(len(ids)) + `) ORDER BY server_id,module_id,target_kind,target_name`
	rows, err := tx.QueryContext(ctx, query, ids...)
	if err != nil {
		return fmt.Errorf("updater: read policy history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var serverID, moduleID, targetKind, targetName, kind, state, params, updated string
		var revision string
		var errorValue sql.NullString
		if err := rows.Scan(&serverID, &moduleID, &targetKind, &targetName, &kind, &state, &params, &revision, &updated, &errorValue); err != nil {
			return err
		}
		revisionN, err := strconv.ParseUint(revision, 10, 64)
		if err != nil {
			return errors.New("updater: invalid source policy revision")
		}
		updatedAt, err := time.Parse("2006-01-02T15:04:05.000000000Z", updated)
		if err != nil {
			return errors.New("updater: invalid source policy timestamp")
		}
		policy := contracts.ControlPolicy{ID: serverID + ":" + moduleID + ":" + targetKind + ":" + targetName, ServerID: contracts.ServerID(serverID), ModuleID: moduleID, TargetKind: targetKind, TargetName: targetName, Kind: kind, State: contracts.ControlPolicyState(state), Parameters: json.RawMessage(params), Revision: revisionN, UpdatedAt: updatedAt}
		if errorValue.Valid && errorValue.String != "" && errorValue.String != "null" {
			var value contracts.Error
			if err := json.Unmarshal([]byte(errorValue.String), &value); err != nil {
				return errors.New("updater: invalid source policy error")
			}
			policy.Error = &value
		}
		if err := policy.Validate(); err != nil {
			return fmt.Errorf("updater: invalid source policy: %w", err)
		}
		out.Policies = append(out.Policies, policy)
	}
	return rows.Err()
}

func readMigrationTraffic(ctx context.Context, tx *sql.Tx, out *MigrationExport, ids []any) error {
	if len(ids) == 0 {
		return nil
	}
	if ok, err := migrationTableExists(ctx, tx, "traffic_periods"); err != nil {
		return err
	} else if !ok {
		return nil
	}
	query := `SELECT server_id,scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json FROM traffic_periods WHERE server_id IN (` + placeholders(len(ids)) + `) ORDER BY server_id,period_start,scope,direction`
	rows, err := tx.QueryContext(ctx, query, ids...)
	if err != nil {
		return fmt.Errorf("updater: read traffic history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var serverID, scope, from, to, timezone, allowance, direction, counted, continuity, interfaces string
		if err := rows.Scan(&serverID, &scope, &from, &to, &timezone, &allowance, &direction, &counted, &continuity, &interfaces); err != nil {
			return err
		}
		fromAt, err := parseMigrationTime(from)
		if err != nil {
			return err
		}
		toAt, err := parseMigrationTime(to)
		if err != nil {
			return err
		}
		allowanceN, err := strconv.ParseUint(allowance, 10, 64)
		if err != nil {
			return errors.New("updater: invalid source traffic allowance")
		}
		countedN, err := strconv.ParseUint(counted, 10, 64)
		if err != nil {
			return errors.New("updater: invalid source traffic count")
		}
		var iface []string
		if err := json.Unmarshal([]byte(interfaces), &iface); err != nil {
			return errors.New("updater: invalid source traffic interfaces")
		}
		period := contracts.TrafficPeriod{Scope: scope, Interfaces: iface, From: fromAt, To: toAt, Timezone: timezone, AllowanceBytes: allowanceN, Direction: direction, CountedBytes: countedN, Continuity: continuity}
		if err := period.Validate(); err != nil {
			return fmt.Errorf("updater: invalid source traffic period: %w", err)
		}
		out.TrafficPeriods = append(out.TrafficPeriods, ExportTrafficPeriod{ServerID: contracts.ServerID(serverID), Period: period})
	}
	return rows.Err()
}

func readMigrationRollups(ctx context.Context, tx *sql.Tx, out *MigrationExport, ids []any) error {
	if len(ids) == 0 {
		return nil
	}
	ok, err := migrationTableExists(ctx, tx, "metric_rollups")
	if err != nil || !ok {
		return err
	}
	query := `SELECT server_id,metric,bucket_start,bucket_seconds,sample_count,observed_seconds,minimum,maximum,weighted_mean,COALESCE(counter_delta,''),coverage FROM metric_rollups WHERE server_id IN (` + placeholders(len(ids)) + `) ORDER BY server_id,bucket_start,metric,bucket_seconds`
	rows, err := tx.QueryContext(ctx, query, ids...)
	if err != nil {
		return fmt.Errorf("updater: read rollups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ExportRollup
		var start string
		var counter string
		if err := rows.Scan(&r.ServerID, &r.Metric, &start, &r.BucketSeconds, &r.SampleCount, &r.ObservedSeconds, &r.Minimum, &r.Maximum, &r.WeightedMean, &counter, &r.Coverage); err != nil {
			return err
		}
		r.BucketStart, err = parseMigrationTime(start)
		if err != nil {
			return err
		}
		r.CounterDelta = counter
		if err := validateExportRollup(r); err != nil {
			return err
		}
		out.Rollups = append(out.Rollups, r)
	}
	return rows.Err()
}

// ImportMigration validates the complete artifact before changing the
// destination. A hash-backed migration_imports record makes a retry after a
// timeout a no-op, while primary-key checks make overlapping history ranges
// safe to import and reject conflicting data rather than silently replacing it.
func ImportMigration(ctx context.Context, destination *sql.DB, artifact []byte, opts ImportOptions) (ImportResult, error) {
	if destination == nil || len(artifact) == 0 || len(artifact) > maxMigrationBytes {
		return ImportResult{}, errors.New("updater: invalid migration artifact")
	}
	export, err := unwrapMigrationPayload(artifact, opts.Passphrase)
	if err != nil {
		return ImportResult{}, err
	}
	if err := validateMigrationExport(export); err != nil {
		return ImportResult{}, err
	}
	tx, err := destination.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, fmt.Errorf("updater: begin migration import: %w", err)
	}
	defer tx.Rollback()
	// Retries must also validate compatibility: a journal entry cannot authorize
	// a successful result against malformed or newer destination metadata.
	if export.Format == MigrationReplayFormat {
		if err := requireReplaySchema(ctx, tx); err != nil {
			return ImportResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migration_imports (artifact_sha256 TEXT PRIMARY KEY, export_id TEXT NOT NULL, imported_at TEXT NOT NULL)`); err != nil {
		return ImportResult{}, fmt.Errorf("updater: create migration journal: %w", err)
	}
	digest := sha256.Sum256(artifact)
	hash := fmt.Sprintf("%x", digest[:])
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT export_id FROM migration_imports WHERE artifact_sha256=?`, hash).Scan(&existing)
	if err == nil {
		return ImportResult{ExportID: existing, AlreadyApplied: true}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ImportResult{}, err
	}
	for _, table := range []string{"servers", "metric_samples", "coverage_gaps"} {
		if ok, e := migrationTableExists(ctx, tx, table); e != nil {
			return ImportResult{}, e
		} else if !ok {
			return ImportResult{}, fmt.Errorf("updater: destination schema is missing %s", table)
		}
	}
	var result ImportResult
	if export.Format == MigrationReplayFormat {
		result, err = applyReplayMigration(ctx, tx, export, opts)
	} else {
		if len(opts.PreviousArtifact) > 0 {
			return ImportResult{}, errors.New("updater: v1 cannot reconcile a replay-safe tail")
		}
		result, err = applyMigrationRows(ctx, tx, export)
	}
	if err != nil {
		return ImportResult{}, err
	}
	if export.Kind == ExportFullHub {
		if err := applyMigrationAuthority(ctx, tx, export); err != nil {
			return ImportResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO migration_imports(artifact_sha256,export_id,imported_at) VALUES(?,?,?)`, hash, export.ExportID, formatMigrationTime(time.Now())); err != nil {
		return ImportResult{}, fmt.Errorf("updater: record migration import: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, fmt.Errorf("updater: commit migration import: %w", err)
	}
	result.ExportID = export.ExportID
	return result, nil
}

func applyMigrationRows(ctx context.Context, tx *sql.Tx, export MigrationExport) (ImportResult, error) {
	result := ImportResult{Servers: len(export.Servers), Samples: 0, Policies: 0, TrafficPeriods: 0, Rollups: 0}
	for _, server := range export.Servers {
		capabilities, _ := json.Marshal(server.Capabilities)
		var heartbeat any
		if server.LastHeartbeat != nil {
			heartbeat = formatMigrationTime(*server.LastHeartbeat)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO servers(id,name,role,architecture,platform,capabilities_json,version,last_heartbeat,connection_state,freshness_state,freshness_reason,configuration_revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, string(server.ID), server.Name, server.Role, server.Architecture, server.Platform, string(capabilities), server.Version, heartbeat, server.ConnectionState, server.FreshnessState, server.FreshnessReason, strconv.FormatUint(server.ConfigurationRevision, 10))
		if err != nil {
			return result, fmt.Errorf("updater: import server: %w", err)
		}
	}
	for _, sample := range export.Samples {
		values, _ := json.Marshal(sample.Values)
		counters, _ := json.Marshal(sample.Counters)
		units, _ := json.Marshal(sample.Units)
		validity, _ := json.Marshal(sample.Validity)
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT observed_at||char(0)||received_at||char(0)||COALESCE(timestamp_uncertainty,'')||char(0)||values_json||char(0)||COALESCE(counters_json,'{}')||char(0)||COALESCE(units_json,'{}')||char(0)||COALESCE(validity_json,'{}') FROM metric_samples WHERE server_id=? AND collector_epoch=? AND sequence=?`, string(sample.ServerID), string(sample.CollectorEpoch), strconv.FormatUint(sample.Sequence, 10)).Scan(&existing)
		want := formatMigrationTime(sample.ObservedAt) + "\x00" + formatMigrationTime(sample.ReceivedAt) + "\x00" + sample.TimestampUncertainty + "\x00" + string(values) + "\x00" + string(counters) + "\x00" + string(units) + "\x00" + string(validity)
		if err == nil {
			if existing != want {
				return result, errors.New("updater: conflicting sample for stable identity")
			}
			result.Duplicate++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO metric_samples(server_id,collector_epoch,sequence,observed_at,received_at,timestamp_uncertainty,values_json,counters_json,units_json,validity_json) VALUES(?,?,?,?,?,?,?,?,?,?)`, string(sample.ServerID), string(sample.CollectorEpoch), strconv.FormatUint(sample.Sequence, 10), formatMigrationTime(sample.ObservedAt), formatMigrationTime(sample.ReceivedAt), sample.TimestampUncertainty, string(values), string(counters), string(units), string(validity)); err != nil {
			return result, fmt.Errorf("updater: import sample: %w", err)
		}
		result.Samples++
	}
	for _, gap := range export.Gaps {
		_, err := tx.ExecContext(ctx, `INSERT INTO coverage_gaps(server_id,collector_epoch,from_sequence,to_sequence,reason) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, string(gap.ServerID), string(gap.Gap.CollectorEpoch), strconv.FormatUint(gap.Gap.FromSequence, 10), strconv.FormatUint(gap.Gap.ToSequence, 10), gap.Gap.Reason)
		if err != nil {
			return result, fmt.Errorf("updater: import coverage gap: %w", err)
		}
	}
	for _, policy := range export.Policies {
		params := string(policy.Parameters)
		if params == "" {
			params = "{}"
		}
		var errorJSON any
		if policy.Error != nil {
			b, _ := json.Marshal(policy.Error)
			errorJSON = string(b)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO control_policies(server_id,module_id,target_kind,target_name,kind,state,parameters_json,revision,updated_at,error_json) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, string(policy.ServerID), policy.ModuleID, policy.TargetKind, policy.TargetName, policy.Kind, string(policy.State), params, policy.Revision, formatMigrationTime(policy.UpdatedAt), errorJSON)
		if err != nil {
			return result, fmt.Errorf("updater: import policy: %w", err)
		}
		result.Policies++
	}
	for _, exportedPeriod := range export.TrafficPeriods {
		period := exportedPeriod.Period
		interfaces, _ := json.Marshal(period.Interfaces)
		serverID := exportedPeriod.ServerID
		if serverID == "" {
			return result, errors.New("updater: traffic period server identity is unavailable")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO traffic_periods(server_id,scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, string(serverID), period.Scope, formatMigrationTime(period.From), formatMigrationTime(period.To), period.Timezone, strconv.FormatUint(period.AllowanceBytes, 10), period.Direction, strconv.FormatUint(period.CountedBytes, 10), period.Continuity, string(interfaces))
		if err != nil {
			return result, fmt.Errorf("updater: import traffic period: %w", err)
		}
		result.TrafficPeriods++
	}
	if len(export.Rollups) > 0 {
		ok, err := migrationTableExists(ctx, tx, "metric_rollups")
		if err != nil {
			return result, err
		}
		if !ok {
			return result, errors.New("updater: destination schema is missing metric_rollups")
		}
		for _, rollup := range export.Rollups {
			_, err := tx.ExecContext(ctx, `INSERT INTO metric_rollups(server_id,metric,bucket_start,bucket_seconds,sample_count,observed_seconds,minimum,maximum,weighted_mean,counter_delta,coverage) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, string(rollup.ServerID), rollup.Metric, formatMigrationTime(rollup.BucketStart), rollup.BucketSeconds, rollup.SampleCount, rollup.ObservedSeconds, rollup.Minimum, rollup.Maximum, rollup.WeightedMean, rollup.CounterDelta, rollup.Coverage)
			if err != nil {
				return result, fmt.Errorf("updater: import rollup: %w", err)
			}
			result.Rollups++
		}
	}
	return result, nil
}

func applyMigrationAuthority(ctx context.Context, tx *sql.Tx, export MigrationExport) error {
	for table, state := range map[string][]byte{"browser_auth_state": export.BrowserAuthState, "fleet_identity_state": export.FleetIdentityState} {
		if len(state) == 0 {
			continue
		}
		ok, err := migrationTableExists(ctx, tx, table)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("updater: destination schema is missing %s", table)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(singleton,state_json) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET state_json=excluded.state_json`, state); err != nil {
			return fmt.Errorf("updater: import %s: %w", table, err)
		}
	}
	return nil
}

func validateMigrationExport(export MigrationExport) error {
	if !((export.Format == MigrationFormat && export.SchemaVersion == MigrationSchemaVersion) || (export.Format == MigrationReplayFormat && export.SchemaVersion == MigrationReplaySchemaVersion)) || (export.Kind != ExportSingleServer && export.Kind != ExportFullHub) || export.ExportID == "" || export.CreatedAt.IsZero() || len(export.Servers) == 0 {
		return errors.New("updater: invalid migration export header")
	}
	if export.Kind == ExportSingleServer && (len(export.Servers) != 1 || export.SourceServerID != export.Servers[0].ID) {
		return errors.New("updater: invalid single-server export")
	}
	if export.Kind == ExportSingleServer && (len(export.BrowserAuthState) > 0 || len(export.FleetIdentityState) > 0) {
		return errors.New("updater: single-server export contains hub authority")
	}
	seenServers := map[contracts.ServerID]bool{}
	for _, server := range export.Servers {
		if !validMigrationServerID(server.ID) || seenServers[server.ID] {
			return errors.New("updater: duplicate or invalid server identity")
		}
		seenServers[server.ID] = true
	}
	seenSamples := map[string]string{}
	for _, sample := range export.Samples {
		if err := sample.Validate(); err != nil {
			return fmt.Errorf("updater: invalid imported sample: %w", err)
		}
		if !seenServers[sample.ServerID] {
			return errors.New("updater: sample references an unexported server")
		}
		b, _ := json.Marshal(sample)
		key := string(sample.ServerID) + "\x00" + string(sample.CollectorEpoch) + "\x00" + strconv.FormatUint(sample.Sequence, 10)
		if old, ok := seenSamples[key]; ok && old != string(b) {
			return errors.New("updater: conflicting duplicate sample")
		}
		seenSamples[key] = string(b)
	}
	for _, gap := range export.Gaps {
		if !seenServers[gap.ServerID] {
			return errors.New("updater: gap references an unexported server")
		}
		if err := gap.Gap.Validate(); err != nil {
			return fmt.Errorf("updater: invalid imported gap: %w", err)
		}
	}
	for _, policy := range export.Policies {
		if !seenServers[policy.ServerID] {
			return errors.New("updater: policy references an unexported server")
		}
		if err := policy.Validate(); err != nil {
			return fmt.Errorf("updater: invalid imported policy: %w", err)
		}
	}
	for _, exportedPeriod := range export.TrafficPeriods {
		if !seenServers[exportedPeriod.ServerID] {
			return errors.New("updater: traffic period references an unexported server")
		}
		if err := exportedPeriod.Period.Validate(); err != nil {
			return fmt.Errorf("updater: invalid imported traffic period: %w", err)
		}
	}
	for _, rollup := range export.Rollups {
		if !seenServers[rollup.ServerID] {
			return errors.New("updater: rollup references an unexported server")
		}
		if err := validateExportRollup(rollup); err != nil {
			return err
		}
	}
	if export.Format == MigrationReplayFormat {
		return validateReplayExport(export)
	}
	if len(export.ReplayState) > 0 || export.RetirementSaturated != nil {
		return errors.New("updater: v1 contains unsupported replay authority")
	}
	return nil
}

func wrapMigrationPayload(payload []byte, kind ExportKind, passphrase string) ([]byte, error) {
	var header struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return nil, err
	}
	if header.Format != MigrationFormat && header.Format != MigrationReplayFormat {
		return nil, errors.New("updater: invalid migration payload format")
	}
	envelope := migrationEnvelope{Format: header.Format, Kind: kind}
	if kind == ExportSingleServer && passphrase == "" {
		envelope.Payload = base64.RawStdEncoding.EncodeToString(payload)
		return json.Marshal(envelope)
	}
	if len(passphrase) < 12 {
		return nil, errors.New("updater: encrypted migration export requires a passphrase of at least 12 characters")
	}
	salt := make([]byte, 16)
	nonceReader := rand.Reader
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	iterations := MigrationKDFIterations
	key, err := migrationKey(passphrase, salt, iterations)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(nonceReader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, payload, migrationAAD(envelope.Format, kind, iterations))
	envelope.Encrypted = true
	envelope.KDF = MigrationKDF
	envelope.Iterations = iterations
	envelope.Salt = base64.RawStdEncoding.EncodeToString(salt)
	envelope.Nonce = base64.RawStdEncoding.EncodeToString(nonce)
	envelope.Payload = base64.RawStdEncoding.EncodeToString(ciphertext)
	return json.Marshal(envelope)
}

func unwrapMigrationPayload(artifact []byte, passphrase string) (MigrationExport, error) {
	var envelope migrationEnvelope
	if err := json.Unmarshal(artifact, &envelope); err != nil || (envelope.Format != MigrationFormat && envelope.Format != MigrationReplayFormat) || (envelope.Kind != ExportSingleServer && envelope.Kind != ExportFullHub) {
		return MigrationExport{}, errors.New("updater: invalid migration envelope")
	}
	encoded, err := base64.RawStdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return MigrationExport{}, errors.New("updater: invalid migration payload")
	}
	if envelope.Encrypted {
		if len(passphrase) < 12 {
			return MigrationExport{}, errors.New("updater: passphrase is required for encrypted migration export")
		}
		if envelope.KDF != MigrationKDF || envelope.Iterations < MigrationMinKDFIterations || envelope.Iterations > MigrationMaxKDFIterations {
			return MigrationExport{}, errors.New("updater: invalid migration key-derivation parameters")
		}
		salt, e1 := base64.RawStdEncoding.DecodeString(envelope.Salt)
		nonce, e2 := base64.RawStdEncoding.DecodeString(envelope.Nonce)
		if e1 != nil || e2 != nil || len(salt) < 16 {
			return MigrationExport{}, errors.New("updater: invalid migration encryption metadata")
		}
		key, err := migrationKey(passphrase, salt, envelope.Iterations)
		if err != nil {
			return MigrationExport{}, err
		}
		block, err := aes.NewCipher(key[:])
		if err != nil {
			return MigrationExport{}, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return MigrationExport{}, err
		}
		encoded, err = gcm.Open(nil, nonce, encoded, migrationAAD(envelope.Format, envelope.Kind, envelope.Iterations))
		if err != nil {
			return MigrationExport{}, errors.New("updater: migration decryption failed")
		}
	} else if envelope.Kind == ExportFullHub || envelope.KDF != "" || envelope.Iterations != 0 || envelope.Salt != "" || envelope.Nonce != "" {
		return MigrationExport{}, errors.New("updater: full-hub export must be encrypted")
	}
	var export MigrationExport
	if err := json.Unmarshal(encoded, &export); err != nil {
		return MigrationExport{}, errors.New("updater: invalid migration document")
	}
	if export.Format != envelope.Format || export.Kind != envelope.Kind {
		return MigrationExport{}, errors.New("updater: migration envelope/payload mismatch")
	}
	return export, nil
}

func migrationKey(passphrase string, salt []byte, iterations int) ([32]byte, error) {
	if iterations < MigrationMinKDFIterations || iterations > MigrationMaxKDFIterations {
		return [32]byte{}, errors.New("updater: migration key-derivation iterations are outside bounds")
	}
	derived, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return [32]byte{}, err
	}
	var key [32]byte
	copy(key[:], derived)
	return key, nil
}
func migrationAAD(format string, kind ExportKind, iterations int) []byte {
	return []byte(fmt.Sprintf("%s/%s/%s/%d", format, kind, MigrationKDF, iterations))
}
func randomExportID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("updater: generate migration export id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func validMigrationServerID(id contracts.ServerID) bool {
	return migrationServerID.MatchString(string(id))
}
func placeholders(n int) string { return strings.TrimRight(strings.Repeat("?,", n), ",") }
func migrationTableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
		return false, err
	}
	return count == 1, nil
}
func formatMigrationTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
func parseMigrationTime(value string) (time.Time, error) {
	t, err := time.Parse("2006-01-02T15:04:05.000000000Z", value)
	if err != nil {
		return time.Time{}, errors.New("updater: invalid migration timestamp")
	}
	return t, nil
}

func scanMigrationServer(row interface{ Scan(...any) error }) (contracts.Server, error) {
	var server contracts.Server
	var id, caps, revision string
	var heartbeat sql.NullString
	if err := row.Scan(&id, &server.Name, &server.Role, &server.Architecture, &server.Platform, &caps, &server.Version, &heartbeat, &server.ConnectionState, &server.FreshnessState, &server.FreshnessReason, &revision); err != nil {
		return server, err
	}
	server.ID = contracts.ServerID(id)
	if err := json.Unmarshal([]byte(caps), &server.Capabilities); err != nil {
		return server, errors.New("updater: invalid source server capabilities")
	}
	if heartbeat.Valid && heartbeat.String != "" {
		value, err := parseMigrationTime(heartbeat.String)
		if err != nil {
			return server, err
		}
		server.LastHeartbeat = &value
	}
	value, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return server, errors.New("updater: invalid source server revision")
	}
	server.ConfigurationRevision = value
	return server, nil
}

func parseMigrationSample(sample *contracts.MetricSample, serverID, epoch, sequence, observed, received, uncertainty, values, counters, units, validity string) error {
	value, err := strconv.ParseUint(sequence, 10, 64)
	if err != nil {
		return errors.New("updater: invalid source sample sequence")
	}
	observedAt, err := parseMigrationTime(observed)
	if err != nil {
		return err
	}
	receivedAt, err := parseMigrationTime(received)
	if err != nil {
		return err
	}
	sample.ServerID, sample.CollectorEpoch, sample.Sequence = contracts.ServerID(serverID), contracts.CollectorEpoch(epoch), value
	sample.ObservedAt, sample.ReceivedAt, sample.TimestampUncertainty = observedAt, receivedAt, uncertainty
	if err := json.Unmarshal([]byte(values), &sample.Values); err != nil {
		return errors.New("updater: invalid source sample values")
	}
	if err := json.Unmarshal([]byte(counters), &sample.Counters); err != nil {
		return errors.New("updater: invalid source sample counters")
	}
	if err := json.Unmarshal([]byte(units), &sample.Units); err != nil {
		return errors.New("updater: invalid source sample units")
	}
	if err := json.Unmarshal([]byte(validity), &sample.Validity); err != nil {
		return errors.New("updater: invalid source sample validity")
	}
	if err := sample.Validate(); err != nil {
		return fmt.Errorf("updater: invalid source sample: %w", err)
	}
	return nil
}

func validateExportRollup(r ExportRollup) error {
	if r.ServerID == "" || r.Metric == "" || r.BucketStart.IsZero() || (r.BucketSeconds != 60 && r.BucketSeconds != 3600) || r.SampleCount < 1 || r.ObservedSeconds < 0 || r.Coverage == "" {
		return errors.New("updater: invalid export rollup")
	}
	return nil
}

// MigrationInfo describes a migration artifact without applying it.
type MigrationInfo struct {
	Format         string
	Kind           ExportKind
	ExportID       string
	SourceServerID contracts.ServerID
	ServerIDs      []contracts.ServerID
}

// InspectMigration validates an unencrypted artifact and reports which servers it
// would write, so a receiving peer can refuse content outside what it was granted
// before importing anything. Encrypted full-hub artifacts cannot be inspected
// without their passphrase and are refused.
func InspectMigration(artifact []byte) (MigrationInfo, error) {
	if len(artifact) == 0 || len(artifact) > maxMigrationBytes {
		return MigrationInfo{}, errors.New("updater: invalid migration artifact")
	}
	export, err := unwrapMigrationPayload(artifact, "")
	if err != nil {
		return MigrationInfo{}, err
	}
	if err := validateMigrationExport(export); err != nil {
		return MigrationInfo{}, err
	}
	info := MigrationInfo{Format: export.Format, Kind: export.Kind, ExportID: export.ExportID, SourceServerID: export.SourceServerID}
	for _, server := range export.Servers {
		info.ServerIDs = append(info.ServerIDs, server.ID)
	}
	return info, nil
}
