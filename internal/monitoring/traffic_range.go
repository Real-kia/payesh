package monitoring

import (
	"context"
	"errors"
	"math/big"
	"strconv"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// TrafficRange summarizes only the two host billing counters. Whole-hour
// bounds match retained rollups; deltas belong to their ending sample's hour.
// Missing/reset buckets are excluded, never converted into zero usage.
type TrafficRange struct {
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	DownloadBytes  *string   `json:"download_bytes"`
	UploadBytes    *string   `json:"upload_bytes"`
	TotalBytes     *string   `json:"total_bytes"`
	DownloadHours  int       `json:"download_hours"`
	UploadHours    int       `json:"upload_hours"`
	RequestedHours int       `json:"requested_hours"`
}

func (s *Store) QueryTrafficRange(ctx context.Context, id contracts.ServerID, from, to time.Time) (TrafficRange, error) {
	if !validStoreServerID(id) || from.IsZero() || !to.After(from) || to.Sub(from) > 90*24*time.Hour || !from.Equal(from.Truncate(time.Hour)) || !to.Equal(to.Truncate(time.Hour)) {
		return TrafficRange{}, errors.New("choose whole-hour bounds spanning at most 90 days")
	}
	var pending int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM rollup_rebuild_queue WHERE server_id=?`, string(id)).Scan(&pending); err != nil {
		return TrafficRange{}, err
	}
	if pending > 0 {
		return TrafficRange{}, ErrRollupsPending
	}
	result := TrafficRange{From: from.UTC(), To: to.UTC(), RequestedHours: int(to.Sub(from) / time.Hour)}
	rows, err := s.db.QueryContext(ctx, `SELECT metric,counter_delta FROM metric_rollups WHERE server_id=? AND metric IN ('net.billing.rx_bytes','net.billing.tx_bytes') AND bucket_seconds=3600 AND bucket_start>=? AND bucket_start<? AND coverage='complete' AND counter_delta<>'' ORDER BY metric,bucket_start`, string(id), FormatPersistedTime(from), FormatPersistedTime(to))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var rx, tx big.Int
	for rows.Next() {
		var metric, raw string
		if err := rows.Scan(&metric, &raw); err != nil {
			return result, err
		}
		if !decimalCounter(raw) {
			return result, errors.New("invalid stored traffic counter")
		}
		value, _ := new(big.Int).SetString(raw, 10)
		if metric == "net.billing.rx_bytes" {
			rx.Add(&rx, value)
			result.DownloadHours++
		} else {
			tx.Add(&tx, value)
			result.UploadHours++
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if result.DownloadHours > 0 {
		value := rx.String()
		result.DownloadBytes = &value
	}
	if result.UploadHours > 0 {
		value := tx.String()
		result.UploadBytes = &value
	}
	if result.DownloadHours > 0 && result.UploadHours > 0 {
		value := new(big.Int).Add(&rx, &tx).String()
		result.TotalBytes = &value
	}
	return result, nil
}

func decimalCounter(raw string) bool {
	value, err := strconv.ParseUint(raw, 10, 64)
	return err == nil && strconv.FormatUint(value, 10) == raw
}
