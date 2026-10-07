package updater

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/dbschema"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// ExportReplayTable is a fixed schema-6 retained-state projection. Table and
// column names are selected locally from the allowlist, never from SQL in an
// artifact. Nullable cells preserve coverage's unknown observed-time bounds.
type ExportReplayTable struct {
	Table string      `json:"table"`
	Rows  [][]*string `json:"rows"`
}

type replayTableSpec struct {
	name, columns, kinds string
	keys                 int
}

// These tables carry replay/derived-processing/configuration correctness only.
// Accounts, credentials, enrollment, jobs and executable action payloads are
// deliberately excluded. Policies/periods/rollups use their typed v1 fields.
var replayTables = []replayTableSpec{
	{"coverage_gaps", "server_id,collector_epoch,from_sequence,to_sequence,reason,from_observed_at,to_observed_at", "ssuusnn", 5},
	{"sequence_frontiers", "server_id,collector_epoch,frontier,max_point", "ssub", 2},
	{"collector_epoch_metadata", "server_id,collector_epoch,first_seen_at,last_seen_at", "sstt", 2},
	{"collector_epoch_retirements", "server_id,collector_epoch,retired_at", "sst", 2},
	{"metric_sample_tombstones", "server_id,collector_epoch,from_sequence,to_sequence", "ssuu", 4},
	{"traffic_usage_ledger", "server_id,collector_epoch,sequence,scope,direction", "ssuss", 5},
	{"traffic_usage_tombstones", "server_id,collector_epoch,scope,direction,from_sequence,to_sequence", "ssssuu", 6},
	{"post_process_queue", "server_id,collector_epoch,sequence,sample_json,created_at", "ssujt", 3},
	{"rollup_rebuild_queue", "server_id,bucket_start,bucket_seconds", "sti", 3},
	{"traffic_allowances", "server_id,scope,direction,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json", "sssjuisj", 3},
	{"traffic_allowance_versions", "server_id,scope,direction,effective_at,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json", "ssstjuisj", 4},
	{"traffic_allowance_changes", "server_id,scope,direction,effective_at,previous_allowance_json,proposed_allowance_json", "ssstjj", 3},
	{"traffic_allowance_requests", "server_id,idempotency_key,request_hash,result_json,created_at", "sssjt", 2},
}

func requireReplaySchema(ctx context.Context, tx *sql.Tx) error {
	version, err := dbschema.ReadSchemaVersionTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("updater: replay migration requires registered schema %d: %w", monitoring.CurrentSchemaVersion, err)
	}
	// The replay allowlist excludes per-server authority rows: authority is a
	// property of one store and is initialized explicitly on the destination.
	if version != monitoring.CurrentSchemaVersion {
		return fmt.Errorf("updater: replay migration requires registered schema %d", monitoring.CurrentSchemaVersion)
	}
	return nil
}

func readReplayState(ctx context.Context, tx *sql.Tx, out *MigrationExport, ids []any) error {
	if err := requireReplaySchema(ctx, tx); err != nil {
		return err
	}
	var saturated int
	if err := tx.QueryRowContext(ctx, `SELECT saturated FROM collector_epoch_retirement_authority WHERE singleton=1`).Scan(&saturated); err != nil {
		return err
	}
	value := saturated != 0
	out.RetirementSaturated = &value
	for _, spec := range replayTables {
		names := strings.Split(spec.columns, ",")
		query := `SELECT ` + spec.columns + ` FROM ` + spec.name + ` WHERE server_id IN (` + placeholders(len(ids)) + `) ORDER BY ` + strings.Join(names[:spec.keys], ",")
		rows, err := tx.QueryContext(ctx, query, ids...)
		if err != nil {
			return fmt.Errorf("updater: read replay state %s: %w", spec.name, err)
		}
		table := ExportReplayTable{Table: spec.name, Rows: make([][]*string, 0)}
		for rows.Next() {
			cells := make([]sql.NullString, len(names))
			scans := make([]any, len(names))
			for i := range cells {
				scans[i] = &cells[i]
			}
			if err := rows.Scan(scans...); err != nil {
				rows.Close()
				return err
			}
			row := make([]*string, len(cells))
			for i, cell := range cells {
				if cell.Valid {
					v := cell.String
					row[i] = &v
				}
			}
			table.Rows = append(table.Rows, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		out.ReplayState = append(out.ReplayState, table)
	}
	return nil
}

func replaySpecs() map[string]replayTableSpec {
	out := make(map[string]replayTableSpec)
	for _, s := range replayTables {
		s.kinds = strings.ReplaceAll(s.kinds, " ", "")
		out[s.name] = s
	}
	return out
}
func replayKey(row []*string, n int) string { data, _ := json.Marshal(row[:n]); return string(data) }

func validateReplayExport(export MigrationExport) error {
	if export.RetirementSaturated == nil || len(export.ReplayState) != len(replayTables) {
		return errors.New("updater: incomplete replay authority")
	}
	if len(export.BrowserAuthState) > 0 || len(export.FleetIdentityState) > 0 {
		return errors.New("updater: replay history artifact contains execution authority")
	}
	servers := make(map[string]bool)
	for _, s := range export.Servers {
		servers[string(s.ID)] = true
	}
	specs := replaySpecs()
	seen := make(map[string]bool)
	for _, table := range export.ReplayState {
		spec, ok := specs[table.Table]
		if !ok || seen[table.Table] || table.Rows == nil {
			return errors.New("updater: invalid replay table allowlist")
		}
		seen[table.Table] = true
		keys := make(map[string]bool)
		for _, row := range table.Rows {
			if len(row) != len(spec.kinds) || row[0] == nil || !servers[*row[0]] {
				return errors.New("updater: replay row references unknown server or columns")
			}
			for i, kind := range spec.kinds {
				cell := row[i]
				if cell == nil {
					if kind == 'n' {
						continue
					}
					return errors.New("updater: invalid null replay cell")
				}
				switch kind {
				case 'u':
					v, err := strconv.ParseUint(*cell, 10, 64)
					if err != nil || strconv.FormatUint(v, 10) != *cell {
						return errors.New("updater: invalid replay sequence/count")
					}
				case 'i':
					v, err := strconv.ParseInt(*cell, 10, 64)
					if err != nil || v < 0 {
						return errors.New("updater: invalid replay integer")
					}
				case 'b':
					if *cell != "0" && *cell != "1" {
						return errors.New("updater: invalid replay boolean")
					}
				case 't', 'n':
					if _, err := parseMigrationTime(*cell); err != nil {
						return err
					}
				case 'j':
					if !json.Valid([]byte(*cell)) || len(*cell) > contracts.MaxEnvelopeBytes {
						return errors.New("updater: invalid replay JSON")
					}
				case 's':
					if len(*cell) == 0 || len(*cell) > 4096 {
						return errors.New("updater: invalid replay identifier")
					}
				}
			}
			key := replayKey(row, spec.keys)
			if keys[key] {
				return errors.New("updater: duplicate replay identity")
			}
			keys[key] = true
			if table.Table == "metric_sample_tombstones" || table.Table == "traffic_usage_tombstones" || table.Table == "coverage_gaps" {
				from, to := 2, 3
				if table.Table == "traffic_usage_tombstones" {
					from, to = 4, 5
				}
				a, _ := strconv.ParseUint(*row[from], 10, 64)
				b, _ := strconv.ParseUint(*row[to], 10, 64)
				if a > b {
					return errors.New("updater: invalid replay range")
				}
			}
			if table.Table == "post_process_queue" {
				var sample contracts.MetricSample
				if err := json.Unmarshal([]byte(*row[3]), &sample); err != nil {
					return err
				}
				if err := sample.Validate(); err != nil {
					return err
				}
				if string(sample.ServerID) != *row[0] || string(sample.CollectorEpoch) != *row[1] || strconv.FormatUint(sample.Sequence, 10) != *row[2] {
					return errors.New("updater: queue sample identity conflict")
				}
			}
		}
	}
	gapKeys := make(map[string]bool)
	for _, gap := range export.Gaps {
		cells := []string{string(gap.ServerID), string(gap.Gap.CollectorEpoch), strconv.FormatUint(gap.Gap.FromSequence, 10), strconv.FormatUint(gap.Gap.ToSequence, 10), gap.Gap.Reason}
		row := make([]*string, len(cells))
		for i := range cells {
			row[i] = &cells[i]
		}
		key := replayKey(row, len(row))
		if gapKeys[key] {
			return errors.New("updater: duplicate coverage identity")
		}
		gapKeys[key] = true
	}
	replayGaps := replayRows(export, "coverage_gaps")
	if len(replayGaps) != len(gapKeys) {
		return errors.New("updater: coverage/replay projection mismatch")
	}
	for _, row := range replayGaps {
		if !gapKeys[replayKey(row, 5)] {
			return errors.New("updater: coverage/replay projection mismatch")
		}
	}
	return validateReplayFrontiers(export)
}

type replayInterval struct{ from, to uint64 }

func streamKey(server, epoch string) string { return server + "\x00" + epoch }
func replayRows(export MigrationExport, table string) [][]*string {
	for _, t := range export.ReplayState {
		if t.Table == table {
			return t.Rows
		}
	}
	return nil
}
func coveredByReplayAuthority(export MigrationExport, sample contracts.MetricSample) bool {
	for _, row := range replayRows(export, "collector_epoch_retirements") {
		if *row[0] == string(sample.ServerID) && *row[1] == string(sample.CollectorEpoch) {
			return true
		}
	}
	for _, row := range replayRows(export, "metric_sample_tombstones") {
		if *row[0] != string(sample.ServerID) || *row[1] != string(sample.CollectorEpoch) {
			continue
		}
		a, _ := strconv.ParseUint(*row[2], 10, 64)
		b, _ := strconv.ParseUint(*row[3], 10, 64)
		if sample.Sequence >= a && sample.Sequence <= b {
			return true
		}
	}
	return false
}
func validateReplayFrontiers(export MigrationExport) error {
	coverage := make(map[string][]replayInterval)
	for _, sample := range export.Samples {
		if coveredByReplayAuthority(export, sample) {
			return errors.New("updater: raw sample conflicts with replay retirement authority")
		}
		key := streamKey(string(sample.ServerID), string(sample.CollectorEpoch))
		coverage[key] = append(coverage[key], replayInterval{sample.Sequence, sample.Sequence})
	}
	for _, table := range []string{"coverage_gaps", "metric_sample_tombstones"} {
		for _, row := range replayRows(export, table) {
			a, _ := strconv.ParseUint(*row[2], 10, 64)
			b, _ := strconv.ParseUint(*row[3], 10, 64)
			if table == "coverage_gaps" {
				// Gaps are half-open; tombstones are inclusive.
				if b <= a {
					return errors.New("updater: invalid half-open coverage gap")
				}
				b--
			}
			key := streamKey(*row[0], *row[1])
			coverage[key] = append(coverage[key], replayInterval{a, b})
		}
	}
	for _, row := range replayRows(export, "sequence_frontiers") {
		frontier, _ := strconv.ParseUint(*row[2], 10, 64)
		intervals := coverage[streamKey(*row[0], *row[1])]
		sort.Slice(intervals, func(i, j int) bool { return intervals[i].from < intervals[j].from })
		var next uint64
		// max_point tracks the maximum point independently of the contiguous
		// prefix. A sparse stream may retain MAX while earlier holes remain.
		maxPresent := false
		for _, interval := range intervals {
			if interval.to == ^uint64(0) {
				maxPresent = true
			}
		}
		for _, interval := range intervals {
			if interval.from > next {
				break
			}
			if interval.to < next {
				continue
			}
			if interval.to == ^uint64(0) {
				next = interval.to
				break
			}
			next = interval.to + 1
		}
		if frontier > next || (*row[3] == "1" && !maxPresent) {
			return errors.New("updater: unsupported replay acknowledgement frontier")
		}
	}
	return nil
}

func sameReplaySnapshot(a, b MigrationExport) bool {
	// IDs/timestamps describe the artifact; equality describes scoped DB state.
	a.ExportID, b.ExportID = "", ""
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	canonical := func(v *MigrationExport) {
		for i := range v.ReplayState {
			sort.Slice(v.ReplayState[i].Rows, func(a, b int) bool {
				x, _ := json.Marshal(v.ReplayState[i].Rows[a])
				y, _ := json.Marshal(v.ReplayState[i].Rows[b])
				return string(x) < string(y)
			})
		}
		sort.Slice(v.ReplayState, func(i, j int) bool { return v.ReplayState[i].Table < v.ReplayState[j].Table })
	}
	canonical(&a)
	canonical(&b)
	return reflect.DeepEqual(a, b)
}

func applyReplayMigration(ctx context.Context, tx *sql.Tx, export MigrationExport, opts ImportOptions) (ImportResult, error) {
	if err := requireReplaySchema(ctx, tx); err != nil {
		return ImportResult{}, err
	}
	ids := make([]any, 0, len(export.Servers))
	serverIDs := make([]contracts.ServerID, 0, len(export.Servers))
	for _, s := range export.Servers {
		ids = append(ids, string(s.ID))
		serverIDs = append(serverIDs, s.ID)
	}
	var saturated, limit, retired int
	if err := tx.QueryRowContext(ctx, `SELECT saturated,max_rows FROM collector_epoch_retirement_authority WHERE singleton=1`).Scan(&saturated, &limit); err != nil {
		return ImportResult{}, err
	}
	if (saturated != 0) != *export.RetirementSaturated {
		return ImportResult{}, errors.New("updater: schema 6 cannot reconcile differently scoped retirement saturation")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM collector_epoch_retirements WHERE server_id NOT IN (`+placeholders(len(ids))+`)`, ids...).Scan(&retired); err != nil {
		return ImportResult{}, err
	}
	if saturated == 0 && retired+len(replayRows(export, "collector_epoch_retirements")) >= limit {
		return ImportResult{}, errors.New("updater: destination retirement capacity would change unrelated authority")
	}
	if len(opts.PreviousArtifact) > 0 {
		if len(opts.PreviousArtifact) > maxMigrationBytes {
			return ImportResult{}, errors.New("updater: invalid previous artifact")
		}
		previous, err := unwrapMigrationPayload(opts.PreviousArtifact, opts.Passphrase)
		if err != nil {
			return ImportResult{}, err
		}
		if err := validateMigrationExport(previous); err != nil {
			return ImportResult{}, err
		}
		if previous.Format != MigrationReplayFormat || previous.Kind != export.Kind || previous.SourceServerID != export.SourceServerID {
			return ImportResult{}, errors.New("updater: incompatible replay baseline")
		}
		digest := sha256.Sum256(opts.PreviousArtifact)
		var imported string
		if err := tx.QueryRowContext(ctx, `SELECT export_id FROM migration_imports WHERE artifact_sha256=?`, fmt.Sprintf("%x", digest[:])).Scan(&imported); err != nil || imported != previous.ExportID {
			return ImportResult{}, errors.New("updater: baseline artifact was not imported")
		}
		current := MigrationExport{Format: MigrationReplayFormat, SchemaVersion: MigrationReplaySchemaVersion, Kind: export.Kind, SourceServerID: export.SourceServerID}
		if err := readMigrationRows(ctx, tx, &current, ExportOptions{Kind: export.Kind, ServerID: export.SourceServerID, ReplaySafe: true, serverIDs: serverIDs}); err != nil {
			return ImportResult{}, err
		}
		if !sameReplaySnapshot(current, previous) {
			return ImportResult{}, errors.New("updater: destination changed since verified replay baseline")
		}
		if err := validateReplaySuccessor(previous, export); err != nil {
			return ImportResult{}, err
		}
	} else {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE id IN (`+placeholders(len(ids))+`)`, ids...).Scan(&existing); err != nil {
			return ImportResult{}, err
		}
		if existing != 0 {
			return ImportResult{}, errors.New("updater: server identity already exists; explicit replay baseline required")
		}
	}
	// Exact replacement is scoped and authorized by an unchanged baseline.
	// Immutable overlaps were checked before any deletes; all writes are one TX.
	for _, table := range append([]string{"metric_samples", "control_policies", "traffic_periods", "metric_rollups"}, replayTableNames()...) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE server_id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
			return ImportResult{}, err
		}
	}
	rowsExport := export
	rowsExport.Gaps = nil // Replay projection includes the complete observed bounds.
	result, err := applyMigrationRows(ctx, tx, rowsExport)
	if err != nil {
		return result, err
	}
	for _, server := range export.Servers {
		capabilities, _ := json.Marshal(server.Capabilities)
		var heartbeat any
		if server.LastHeartbeat != nil {
			heartbeat = formatMigrationTime(*server.LastHeartbeat)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE servers SET name=?,capabilities_json=?,version=?,last_heartbeat=?,connection_state=?,freshness_state=?,freshness_reason=?,configuration_revision=? WHERE id=?`, server.Name, string(capabilities), server.Version, heartbeat, server.ConnectionState, server.FreshnessState, server.FreshnessReason, strconv.FormatUint(server.ConfigurationRevision, 10), string(server.ID)); err != nil {
			return ImportResult{}, err
		}
	}
	for _, table := range export.ReplayState {
		spec := replaySpecs()[table.Table]
		for _, row := range table.Rows {
			values := make([]any, len(row))
			for i, cell := range row {
				if cell != nil {
					values[i] = *cell
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+spec.name+`(`+spec.columns+`) VALUES(`+placeholders(len(values))+`)`, values...); err != nil {
				return ImportResult{}, fmt.Errorf("updater: apply replay %s: %w", spec.name, err)
			}
		}
	}
	return result, nil
}
func replayTableNames() []string {
	out := make([]string, 0, len(replayTables))
	for _, s := range replayTables {
		out = append(out, s.name)
	}
	return out
}

// Replay authority is monotonic even when retention changes its representation:
// adjacent ranges may merge, ledgers may become ranges, and retirement may
// replace all stream metadata. Coverage gaps and raw samples never substitute
// for a tombstone: neither permanently prevents a replay or a second charge.
func validateReplayAuthoritySuccessor(previous, next MigrationExport) error {
	retired := make(map[string]bool)
	for _, row := range replayRows(next, "collector_epoch_retirements") {
		retired[replayKey(row, 2)] = true
	}
	raw, charged := make(map[string][]replayInterval), make(map[string][]replayInterval)
	for _, row := range replayRows(next, "metric_sample_tombstones") {
		from, _ := strconv.ParseUint(*row[2], 10, 64)
		to, _ := strconv.ParseUint(*row[3], 10, 64)
		key := replayKey(row, 2)
		raw[key] = append(raw[key], replayInterval{from, to})
	}
	for _, row := range replayRows(next, "traffic_usage_tombstones") {
		from, _ := strconv.ParseUint(*row[4], 10, 64)
		to, _ := strconv.ParseUint(*row[5], 10, 64)
		key := replayKey(row, 4)
		charged[key] = append(charged[key], replayInterval{from, to})
	}
	for _, row := range replayRows(next, "traffic_usage_ledger") {
		seq, _ := strconv.ParseUint(*row[2], 10, 64)
		key := replayKey([]*string{row[0], row[1], row[3], row[4]}, 4)
		charged[key] = append(charged[key], replayInterval{seq, seq})
	}
	for _, coverage := range []map[string][]replayInterval{raw, charged} {
		for key, intervals := range coverage {
			sort.Slice(intervals, func(i, j int) bool { return intervals[i].from < intervals[j].from })
			merged := intervals[:0]
			for _, interval := range intervals {
				if len(merged) == 0 {
					merged = append(merged, interval)
					continue
				}
				last := &merged[len(merged)-1]
				if interval.from <= last.to || (last.to != ^uint64(0) && interval.from == last.to+1) {
					if interval.to > last.to {
						last.to = interval.to
					}
				} else {
					merged = append(merged, interval)
				}
			}
			coverage[key] = merged
		}
	}
	for _, name := range []string{"metric_sample_tombstones", "traffic_usage_tombstones", "traffic_usage_ledger"} {
		for _, row := range replayRows(previous, name) {
			if retired[replayKey(row, 2)] {
				continue
			}
			coverage, key := raw, replayKey(row, 2)
			fromIndex, toIndex := 2, 3
			if name == "traffic_usage_tombstones" {
				coverage, key = charged, replayKey(row, 4)
				fromIndex, toIndex = 4, 5
			} else if name == "traffic_usage_ledger" {
				coverage, key = charged, replayKey([]*string{row[0], row[1], row[3], row[4]}, 4)
				fromIndex, toIndex = 2, 2
			}
			from, _ := strconv.ParseUint(*row[fromIndex], 10, 64)
			to, _ := strconv.ParseUint(*row[toIndex], 10, 64)
			intervals := coverage[key]
			i := sort.Search(len(intervals), func(i int) bool { return intervals[i].to >= from })
			if i == len(intervals) || intervals[i].from > from || intervals[i].to < to {
				return fmt.Errorf("updater: replay successor removed committed %s authority", name)
			}
		}
	}
	return nil
}

func validateReplaySuccessor(previous, next MigrationExport) error {
	if err := validateReplayAuthoritySuccessor(previous, next); err != nil {
		return err
	}

	if len(previous.Servers) != len(next.Servers) {
		return errors.New("updater: replay successor changed server identities")
	}
	for i, s := range previous.Servers {
		n := next.Servers[i]
		if s.ID != n.ID || s.Role != n.Role || s.Architecture != n.Architecture || s.Platform != n.Platform {
			return errors.New("updater: replay successor changed immutable server identity")
		}
	}
	samples := make(map[string]contracts.MetricSample)
	for _, sample := range next.Samples {
		samples[streamKey(string(sample.ServerID), string(sample.CollectorEpoch))+"\x00"+strconv.FormatUint(sample.Sequence, 10)] = sample
	}
	for _, sample := range previous.Samples {
		n, ok := samples[streamKey(string(sample.ServerID), string(sample.CollectorEpoch))+"\x00"+strconv.FormatUint(sample.Sequence, 10)]
		if ok && !reflect.DeepEqual(sample, n) {
			return errors.New("updater: conflicting immutable sample in replay successor")
		}
		if !ok && !coveredByReplayAuthority(next, sample) {
			return errors.New("updater: removed sample lacks replay authority")
		}
	}
	policies := make(map[string]contracts.ControlPolicy)
	for _, policy := range next.Policies {
		policies[policy.ID] = policy
	}
	for _, old := range previous.Policies {
		if policy, ok := policies[old.ID]; ok {
			if policy.Revision < old.Revision || (policy.Revision == old.Revision && !reflect.DeepEqual(policy, old)) {
				return errors.New("updater: conflicting immutable policy revision")
			}
		}
	}
	for _, name := range []string{"traffic_allowance_versions", "collector_epoch_retirements"} {
		spec := replaySpecs()[name]
		rows := make(map[string][]*string)
		for _, row := range replayRows(next, name) {
			rows[replayKey(row, spec.keys)] = row
		}
		for _, row := range replayRows(previous, name) {
			n, ok := rows[replayKey(row, spec.keys)]
			if !ok || !reflect.DeepEqual(row, n) {
				return errors.New("updater: changed immutable replay configuration/retirement")
			}
		}
	}
	return nil
}
