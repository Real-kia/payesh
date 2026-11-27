package monitoring

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"
)

const (
	fleetTrafficRetention = 30 * 24 * time.Hour
	fleetFineRetention    = 24 * time.Hour
	fleetFineBucket       = time.Duration(MidRollupSeconds) * time.Second
)

type FleetTrafficHour struct {
	Start           time.Time `json:"start"`
	DownloadBytes   *string   `json:"download_bytes"`
	UploadBytes     *string   `json:"upload_bytes"`
	DownloadServers int       `json:"download_servers"`
	UploadServers   int       `json:"upload_servers"`
	ServerCount     int       `json:"server_count"`
}

type FleetTrafficHistory struct {
	Enabled       bool               `json:"enabled"`
	End           time.Time          `json:"end"`
	Hours         []FleetTrafficHour `json:"hours"`
	FiveMinuteEnd *time.Time         `json:"five_minute_end,omitempty"`
	FiveMinutes   []FleetTrafficHour `json:"five_minutes,omitempty"`
	Tags          []string           `json:"tags,omitempty"`
}

func (s *Store) FleetTrafficEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.readDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fleet_traffic_settings)`).Scan(&enabled)
	return enabled, err
}

func (s *Store) SetFleetTrafficEnabled(ctx context.Context, enabled bool, now time.Time) error {
	if !enabled {
		_, err := s.db.ExecContext(ctx, `DELETE FROM fleet_traffic_settings WHERE singleton=1`)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO fleet_traffic_settings (singleton,next_hour,next_five_minutes) VALUES (1,?,?)`,
		FormatPersistedTime(nextFleetBucket(now, time.Hour)), FormatPersistedTime(nextFleetBucket(now, fleetFineBucket)))
	return err
}

func nextFleetBucket(now time.Time, duration time.Duration) time.Time {
	start := now.UTC().Truncate(duration)
	if start.Before(now) {
		return start.Add(duration)
	}
	return start
}

func (s *Store) FleetTrafficHistory(ctx context.Context, now time.Time) (FleetTrafficHistory, error) {
	return s.FleetTrafficHistoryByTag(ctx, now, "")
}

func (s *Store) FleetTrafficHistoryByTag(ctx context.Context, now time.Time, tag string) (FleetTrafficHistory, error) {
	end := now.UTC().Truncate(time.Hour)
	result := FleetTrafficHistory{End: end, Hours: []FleetTrafficHour{}}
	var err error
	if result.Enabled, err = s.FleetTrafficEnabled(ctx); err != nil {
		return result, err
	}
	if tag == "" {
		rows, err := s.readDB().QueryContext(ctx, `SELECT DISTINCT tag FROM fleet_traffic_tag_hours
			WHERE bucket_start>=? AND bucket_start<? ORDER BY tag`,
			FormatPersistedTime(end.Add(-fleetTrafficRetention)), FormatPersistedTime(end))
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var label string
			if err := rows.Scan(&label); err != nil {
				rows.Close()
				return result, err
			}
			result.Tags = append(result.Tags, label)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return result, err
		}
	}
	query := `SELECT bucket_start,download_bytes,upload_bytes,download_servers,upload_servers,server_count FROM `
	args := []any{FormatPersistedTime(end.Add(-fleetTrafficRetention)), FormatPersistedTime(end)}
	if tag == "" {
		query += "fleet_traffic_hours WHERE "
	} else {
		query += "fleet_traffic_tag_hours WHERE tag=? AND "
		args = append([]any{strings.ToLower(tag)}, args...)
	}
	result.Hours, err = s.readFleetTrafficPoints(ctx, query+"bucket_start>=? AND bucket_start<? ORDER BY bucket_start", args...)
	if err != nil {
		return result, err
	}
	fineEnd := now.UTC().Truncate(fleetFineBucket)
	result.FiveMinutes, err = s.readFleetTrafficPoints(ctx, `SELECT bucket_start,download_bytes,upload_bytes,download_servers,upload_servers,server_count
		FROM fleet_traffic_five_minutes WHERE tag=? AND bucket_start>=? AND bucket_start<? ORDER BY bucket_start`,
		strings.ToLower(tag), FormatPersistedTime(fineEnd.Add(-fleetFineRetention)), FormatPersistedTime(fineEnd))
	if len(result.FiveMinutes) > 0 {
		result.FiveMinuteEnd = &fineEnd
	}
	return result, err
}

func (s *Store) readFleetTrafficPoints(ctx context.Context, query string, args ...any) ([]FleetTrafficHour, error) {
	rows, err := s.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []FleetTrafficHour{}
	for rows.Next() {
		var point FleetTrafficHour
		var start string
		if err := rows.Scan(&start, &point.DownloadBytes, &point.UploadBytes, &point.DownloadServers, &point.UploadServers, &point.ServerCount); err != nil {
			return nil, err
		}
		point.Start, err = time.Parse(persistedTimeLayout, start)
		if err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

func (s *Store) captureFleetTraffic(ctx context.Context, now time.Time) error {
	return errors.Join(
		s.captureFleetTrafficBuckets(ctx, now, fleetFineBucket, time.Minute, fleetFineRetention,
			"next_five_minutes", "fleet_traffic_five_minutes", "fleet_traffic_five_minutes"),
		s.captureFleetTrafficBuckets(ctx, now, time.Hour, 5*time.Minute, fleetTrafficRetention,
			"next_hour", "fleet_traffic_hours", "fleet_traffic_tag_hours"),
	)
}

// Save a completed bucket and advance the cursor in the same transaction.
func (s *Store) captureFleetTrafficBuckets(ctx context.Context, now time.Time, bucket, grace, retention time.Duration, cursor, fleetTable, tagTable string) error {
	end := now.UTC().Add(-grace).Truncate(bucket)
	cutoff := FormatPersistedTime(now.UTC().Truncate(bucket).Add(-retention))
	var due bool
	err := s.readDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fleet_traffic_settings WHERE `+cursor+`<?)
		OR EXISTS(SELECT 1 FROM `+fleetTable+` WHERE bucket_start<?)`, FormatPersistedTime(end), cutoff).Scan(&due)
	if err != nil || !due {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+fleetTable+` WHERE bucket_start<?`, cutoff); err != nil {
		return err
	}
	if tagTable != fleetTable {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+tagTable+` WHERE bucket_start<?`, cutoff); err != nil {
			return err
		}
	}
	// An existing opt-in predates the fine-resolution cursor. Start with the
	// next full bucket because the old settings do not retain the opt-in time.
	if _, err := tx.ExecContext(ctx, `UPDATE fleet_traffic_settings SET `+cursor+`=? WHERE `+cursor+`=''`,
		FormatPersistedTime(nextFleetBucket(now, bucket))); err != nil {
		return err
	}
	var start string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT MAX(`+cursor+`, ?) FROM fleet_traffic_settings), '')`, cutoff).Scan(&start); err != nil {
		return err
	}
	if start == "" || start >= FormatPersistedTime(end) {
		return tx.Commit()
	}
	first, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return err
	}
	// Bound catch-up work per pass so ingestion can continue between batches.
	batchEnd := FormatPersistedTime(end)
	if limit := FormatPersistedTime(first.Add(24 * bucket)); limit < batchEnd {
		batchEnd = limit
	}
	var serverCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers`).Scan(&serverCount); err != nil {
		return err
	}
	// Snapshot the tags independently of server lifetime. An old hour keeps
	// the same totals and coverage even when a server is renamed or removed.
	serverTags := make(map[string][]string)
	tagCounts := make(map[string]int)
	tags, err := tx.QueryContext(ctx, `SELECT server_id,tag FROM server_tags`)
	if err != nil {
		return err
	}
	for tags.Next() {
		var id, label string
		if err := tags.Scan(&id, &label); err != nil {
			tags.Close()
			return err
		}
		tag := strings.ToLower(label)
		serverTags[id] = append(serverTags[id], tag)
		tagCounts[tag]++
	}
	tags.Close()
	if err := tags.Err(); err != nil {
		return err
	}
	type totals struct {
		bytes [2]big.Int
		count [2]int
	}
	type pointKey struct{ bucket, tag string }
	points := make(map[pointKey]*totals)
	add := func(key pointKey, direction int, value *big.Int) {
		point := points[key]
		if point == nil {
			point = &totals{}
			points[key] = point
		}
		point.bytes[direction].Add(&point.bytes[direction], value)
		point.count[direction]++
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,r.bucket_start,r.metric,r.counter_delta
		FROM servers s CROSS JOIN metric_rollups r
		WHERE r.server_id=s.id AND r.bucket_seconds=? AND r.bucket_start>=? AND r.bucket_start<?
		  AND r.metric IN ('net.billing.rx_bytes','net.billing.tx_bytes') AND r.coverage='complete' AND r.counter_delta<>''
		  AND NOT EXISTS (SELECT 1 FROM rollup_rebuild_queue q WHERE q.server_id=r.server_id
		    AND q.bucket_start=r.bucket_start AND q.bucket_seconds=r.bucket_seconds)`, int(bucket/time.Second), start, batchEnd)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, bucket, metric, raw string
		if err := rows.Scan(&id, &bucket, &metric, &raw); err != nil {
			return err
		}
		if !decimalCounter(raw) {
			continue
		}
		direction := 0
		if metric == "net.billing.tx_bytes" {
			direction = 1
		}
		value, _ := new(big.Int).SetString(raw, 10)
		add(pointKey{bucket: bucket}, direction, value)
		for _, tag := range serverTags[id] {
			add(pointKey{bucket: bucket, tag: tag}, direction, value)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for key, point := range points {
		var bytes [2]any
		for direction, count := range point.count {
			if count > 0 {
				bytes[direction] = point.bytes[direction].String()
			}
		}
		query := `INSERT OR IGNORE INTO fleet_traffic_hours VALUES (?,?,?,?,?,?)`
		args := []any{key.bucket, bytes[0], bytes[1], point.count[0], point.count[1], serverCount}
		if bucket == fleetFineBucket {
			count := serverCount
			if key.tag != "" {
				count = tagCounts[key.tag]
			}
			query = `INSERT OR IGNORE INTO fleet_traffic_five_minutes (bucket_start,tag,download_bytes,upload_bytes,download_servers,upload_servers,server_count) VALUES (?,?,?,?,?,?,?)`
			args = []any{key.bucket, key.tag, bytes[0], bytes[1], point.count[0], point.count[1], count}
		} else if key.tag != "" {
			query = `INSERT OR IGNORE INTO fleet_traffic_tag_hours VALUES (?,?,?,?,?,?,?)`
			args = []any{key.bucket, key.tag, bytes[0], bytes[1], point.count[0], point.count[1], tagCounts[key.tag]}
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fleet_traffic_settings SET `+cursor+`=? WHERE singleton=1`, batchEnd); err != nil {
		return err
	}
	return tx.Commit()
}
