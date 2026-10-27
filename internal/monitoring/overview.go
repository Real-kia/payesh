package monitoring

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/auth"
	"github.com/Real-kia/payesh/internal/contracts"
)

// The overview is everything the dashboard's server cards need in one
// response: current values plus a 15-minute series per server. It replaces
// one metrics request per server, so opening the dashboard over a slow link
// is quick, and it is computed on the hub (never on nodes).

// overviewWindow is the history each card shows; responses are cached for
// metricCacheTTL like other dashboard reads.
const overviewWindow = 15 * time.Minute

// OverviewSeries holds parallel columns; nil marks a missing value.
type OverviewSeries struct {
	T      []int64    `json:"t"`
	CPU    []*float64 `json:"cpu"`
	Memory []*float64 `json:"memory"`
	Swap   []*float64 `json:"swap"`
	Disk   []*float64 `json:"disk"`
	RX     []*float64 `json:"rx"`
	TX     []*float64 `json:"tx"`
}

type OverviewItem struct {
	ServerID   contracts.ServerID `json:"server_id"`
	ObservedAt *time.Time         `json:"observed_at,omitempty"`
	CPU        *float64           `json:"cpu"`
	CPUCores   *int               `json:"cpu_cores,omitempty"`
	Memory     *float64           `json:"memory"`
	MemTotal   *string            `json:"memory_total_bytes,omitempty"`
	MemUsed    *string            `json:"memory_used_bytes,omitempty"`
	Swap       *float64           `json:"swap"`
	SwapTotal  *string            `json:"swap_total_bytes,omitempty"`
	SwapUsed   *string            `json:"swap_used_bytes,omitempty"`
	Disk       *float64           `json:"disk"`
	DiskTotal  *string            `json:"disk_total_bytes,omitempty"`
	DiskUsed   *string            `json:"disk_used_bytes,omitempty"`
	// RX and TX are bytes per second between the last two samples.
	RX     *float64       `json:"rx"`
	TX     *float64       `json:"tx"`
	Series OverviewSeries `json:"series"`
	// Billing is the active traffic period with an allowance, if any.
	Billing *OverviewBilling `json:"billing,omitempty"`
}

type OverviewBilling struct {
	CountedBytes   string `json:"counted_bytes"`
	AllowanceBytes string `json:"allowance_bytes"`
}

type Overview struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Items       []OverviewItem `json:"items"`
}

type overviewRow struct {
	server       string
	epoch        string
	sequence     uint64
	observed     time.Time
	cpu, memory  sql.NullFloat64
	disk, swapPc sql.NullFloat64
	swapUsed     sql.NullString
	swapTotal    sql.NullString
	rx, tx       sql.NullString
	cpuCores     sql.NullFloat64
	memTotal     sql.NullString
	memUsed      sql.NullString
	diskTotal    sql.NullString
	diskUsed     sql.NullString
}

// QueryOverview builds the overview for ids (all servers when empty).
func (s *Store) QueryOverview(ctx context.Context, now time.Time, ids []string) (Overview, error) {
	query := `SELECT server_id,collector_epoch,sequence,observed_at,
 COALESCE(json_extract(values_json,'$."cpu.utilization"'),json_extract(values_json,'$.cpu')),
 COALESCE(json_extract(values_json,'$."memory.used_percent"'),json_extract(values_json,'$.memory')),
 COALESCE(json_extract(values_json,'$."disk.root.used_percent"'),json_extract(values_json,'$."disk.used_percent"'),json_extract(values_json,'$.disk')),
 COALESCE(json_extract(values_json,'$."memory.swap_used_percent"'),json_extract(values_json,'$."swap.used_percent"')),
 json_extract(counters_json,'$."memory.swap_used_bytes"'),json_extract(counters_json,'$."memory.swap_total_bytes"'),
 json_extract(counters_json,'$."net.billing.rx_bytes"'),json_extract(counters_json,'$."net.billing.tx_bytes"'),
 json_extract(values_json,'$."cpu.cores"'),
 json_extract(counters_json,'$."memory.total_bytes"'),json_extract(counters_json,'$."memory.used_bytes"'),
 json_extract(counters_json,'$."disk.root.total_bytes"'),json_extract(counters_json,'$."disk.root.used_bytes"')
FROM metric_samples WHERE observed_at>=?`
	args := []any{FormatPersistedTime(now.Add(-overviewWindow))}
	if len(ids) > 0 {
		query += ` AND server_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += ` ORDER BY server_id, observed_at, length(sequence), sequence`
	rows, err := s.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return Overview{}, err
	}
	defer rows.Close()
	byServer := map[string][]overviewRow{}
	order := []string{}
	for rows.Next() {
		var row overviewRow
		var sequence, observed string
		if err := rows.Scan(&row.server, &row.epoch, &sequence, &observed, &row.cpu, &row.memory, &row.disk, &row.swapPc, &row.swapUsed, &row.swapTotal, &row.rx, &row.tx, &row.cpuCores, &row.memTotal, &row.memUsed, &row.diskTotal, &row.diskUsed); err != nil {
			return Overview{}, err
		}
		if row.sequence, err = strconv.ParseUint(sequence, 10, 64); err != nil {
			continue
		}
		if row.observed, err = time.Parse(time.RFC3339Nano, observed); err != nil {
			continue
		}
		if _, seen := byServer[row.server]; !seen {
			order = append(order, row.server)
		}
		byServer[row.server] = append(byServer[row.server], row)
	}
	if err := rows.Err(); err != nil {
		return Overview{}, err
	}
	billing, err := s.activeBilling(ctx, now)
	if err != nil {
		return Overview{}, err
	}
	sort.Strings(order)
	overview := Overview{GeneratedAt: now.UTC(), Items: make([]OverviewItem, 0, len(order))}
	for _, id := range order {
		item := buildOverviewItem(id, byServer[id])
		item.Billing = billing[id]
		overview.Items = append(overview.Items, item)
	}
	return overview, nil
}

func buildOverviewItem(id string, rows []overviewRow) OverviewItem {
	item := OverviewItem{ServerID: contracts.ServerID(id)}
	series := &item.Series
	for i, row := range rows {
		series.T = append(series.T, row.observed.Unix())
		series.CPU = append(series.CPU, rounded(row.cpu))
		series.Memory = append(series.Memory, rounded(row.memory))
		series.Disk = append(series.Disk, rounded(row.disk))
		series.Swap = append(series.Swap, swapPercent(row))
		var rx, tx *float64
		if i > 0 {
			rx, tx = counterRate(rows[i-1], row, rows[i-1].rx, row.rx), counterRate(rows[i-1], row, rows[i-1].tx, row.tx)
		}
		series.RX = append(series.RX, rx)
		series.TX = append(series.TX, tx)
	}
	if n := len(rows); n > 0 {
		last := rows[n-1]
		observed := last.observed
		item.ObservedAt = &observed
		item.CPU, item.Memory, item.Disk, item.Swap = series.CPU[n-1], series.Memory[n-1], series.Disk[n-1], series.Swap[n-1]
		item.RX, item.TX = series.RX[n-1], series.TX[n-1]
		for i := n - 1; i >= 0; i-- {
			r := rows[i]
			if item.CPUCores == nil && r.cpuCores.Valid && r.cpuCores.Float64 > 0 {
				cores := int(r.cpuCores.Float64)
				item.CPUCores = &cores
			}
			if item.MemTotal == nil && r.memTotal.Valid && r.memTotal.String != "" {
				val := r.memTotal.String
				item.MemTotal = &val
			}
			if item.MemUsed == nil && r.memUsed.Valid && r.memUsed.String != "" {
				val := r.memUsed.String
				item.MemUsed = &val
			}
			if item.SwapTotal == nil && r.swapTotal.Valid && r.swapTotal.String != "" {
				val := r.swapTotal.String
				item.SwapTotal = &val
			}
			if item.SwapUsed == nil && r.swapUsed.Valid && r.swapUsed.String != "" {
				val := r.swapUsed.String
				item.SwapUsed = &val
			}
			if item.DiskTotal == nil && r.diskTotal.Valid && r.diskTotal.String != "" {
				val := r.diskTotal.String
				item.DiskTotal = &val
			}
			if item.DiskUsed == nil && r.diskUsed.Valid && r.diskUsed.String != "" {
				val := r.diskUsed.String
				item.DiskUsed = &val
			}
		}
	}
	return item
}

func rounded(value sql.NullFloat64) *float64 {
	if !value.Valid || math.IsNaN(value.Float64) || math.IsInf(value.Float64, 0) {
		return nil
	}
	v := math.Round(value.Float64*100) / 100
	return &v
}

// swapPercent prefers a reported percentage and otherwise derives it from the
// byte counters agents send. A host without swap has no value.
func swapPercent(row overviewRow) *float64 {
	if row.swapPc.Valid {
		return rounded(row.swapPc)
	}
	used, errUsed := strconv.ParseFloat(row.swapUsed.String, 64)
	total, errTotal := strconv.ParseFloat(row.swapTotal.String, 64)
	if !row.swapUsed.Valid || !row.swapTotal.Valid || errUsed != nil || errTotal != nil || total <= 0 {
		return nil
	}
	return rounded(sql.NullFloat64{Float64: used / total * 100, Valid: true})
}

// counterRate is bytes per second between consecutive samples of one
// collector run; restarts, gaps and counter resets give no value.
func counterRate(previous, current overviewRow, before, after sql.NullString) *float64 {
	if !before.Valid || !after.Valid || previous.epoch != current.epoch || current.sequence != previous.sequence+1 {
		return nil
	}
	b, errB := strconv.ParseUint(before.String, 10, 64)
	a, errA := strconv.ParseUint(after.String, 10, 64)
	seconds := current.observed.Sub(previous.observed).Seconds()
	if errB != nil || errA != nil || a < b || seconds <= 0 {
		return nil
	}
	rate := math.Round(float64(a-b)/seconds*10) / 10
	return &rate
}

// serveOverview answers GET /api/v1/overview, compressed when the client
// accepts gzip and cached briefly per access scope so any number of open
// dashboards costs one query.
func (a *API) serveOverview(w http.ResponseWriter, r *http.Request) {
	ids := auth.AllowedServerIDs(r)
	gz := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	key := "overview?" + strings.Join(ids, ",") + "&gzip=" + strconv.FormatBool(gz)
	a.overviewCache.serve(w, key, func(w http.ResponseWriter) {
		overview, err := a.Store.QueryOverview(r.Context(), time.Now().UTC(), ids)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read the overview", true)
			return
		}
		body, err := json.Marshal(overview)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "encode_error", "could not encode the overview", true)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", "Accept-Encoding")
		if gz {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
			_, _ = zw.Write(body)
			_ = zw.Close()
			w.Header().Set("Content-Encoding", "gzip")
			body = buf.Bytes()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

// activeBilling returns, per server, the current period with an allowance.
func (s *Store) activeBilling(ctx context.Context, now time.Time) (map[string]*OverviewBilling, error) {
	at := FormatPersistedTime(now)
	rows, err := s.readDB().QueryContext(ctx, `SELECT server_id,counted_bytes,allowance_bytes FROM traffic_periods WHERE period_start<=? AND period_end>? AND allowance_bytes<>'0' ORDER BY period_start`, at, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*OverviewBilling{}
	for rows.Next() {
		var id string
		var billing OverviewBilling
		if err := rows.Scan(&id, &billing.CountedBytes, &billing.AllowanceBytes); err != nil {
			return nil, err
		}
		out[id] = &billing
	}
	return out, rows.Err()
}
