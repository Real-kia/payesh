package monitoring

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

type API struct {
	Store           *Store
	Token           string
	authorizef      func(http.ResponseWriter, *http.Request) bool
	trafficForecast http.Handler
}

func NewAPI(store *Store, token string) (*API, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	return &API{Store: store, Token: token}, nil
}

// NewAPIWithAuthorizer exposes the bounded monitoring reads to another
// authentication boundary (the browser session service in package 04). The
// caller remains responsible for authenticated routing and CSRF on mutations.
func NewAPIWithAuthorizer(store *Store, authorize func(http.ResponseWriter, *http.Request) bool) (*API, error) {
	if store == nil || authorize == nil {
		return nil, errors.New("store and authorizer are required")
	}
	return &API{Store: store, authorizef: authorize}, nil
}

// SetTrafficForecastHandler attaches the package-05 forecast read to the
// bearer-token local API without making monitoring import the traffic package.
// The monitoring API still owns authentication and only delegates the exact
// read-only forecast route; traffic configuration remains behind the browser
// session/CSRF boundary in package fleet.
func (a *API) SetTrafficForecastHandler(handler http.Handler) {
	if a != nil {
		a.trafficForecast = handler
	}
}

func (a *API) Handler() http.Handler {
	return http.HandlerFunc(a.serveHTTP)
}

func (a *API) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !a.authorize(w, r) {
		return
	}
	if a.trafficForecast != nil && r.Method == http.MethodGet && isTrafficForecastPath(r.URL.Path) {
		a.trafficForecast.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is available in package 03", false)
		return
	}
	if r.URL.Path == "/api/v1/servers" {
		a.listServers(w, r)
		return
	}
	const prefix = "/api/v1/servers/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	serverID := contracts.ServerID(parts[0])
	if len(serverID) < 16 || len(serverID) > 128 || !isSafeServerID(string(serverID)) {
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	if len(parts) == 1 {
		a.getServer(w, r, serverID)
		return
	}
	switch parts[1] {
	case "metrics":
		if len(parts) != 2 {
			writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
			return
		}
		a.queryMetrics(w, r, serverID)
	case "traffic":
		if len(parts) != 2 {
			writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
			return
		}
		a.queryTraffic(w, r, serverID)
	case "logs":
		if len(parts) == 3 && parts[2] == "sources" {
			a.listLogSources(w, r, serverID)
			return
		}
		if len(parts) == 3 && parts[2] == "live" {
			a.tailLogs(w, r, serverID)
			return
		}
		if len(parts) != 2 {
			writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
			return
		}
		a.queryLogs(w, r, serverID)
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
	}
}

func isTrafficForecastPath(path string) bool {
	const prefix = "/api/v1/servers/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, prefix), "/"), "/")
	return len(parts) == 3 && parts[1] == "traffic" && parts[2] == "forecast"
}

func (a *API) getServer(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	if err := a.Store.RefreshServerStates(r.Context(), time.Now().UTC()); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not refresh server state", true)
		return
	}
	server, found, err := a.Store.GetServer(r.Context(), serverID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read server", true)
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (a *API) authorize(w http.ResponseWriter, r *http.Request) bool {
	if a.authorizef != nil {
		return a.authorizef(w, r)
	}
	if a.Token == "" {
		writeAPIError(w, http.StatusServiceUnavailable, "auth_not_configured", "protected local API is disabled until a token is configured", true)
		return false
	}
	value := ""
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		value = parts[1]
	}
	if value == "" || subtle.ConstantTimeCompare([]byte(value), []byte(a.Token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="payesh-local"`)
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
		return false
	}
	return true
}

func (a *API) listServers(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.RefreshServerStates(r.Context(), time.Now().UTC()); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not refresh server state", true)
		return
	}
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	page, err := a.Store.QueryServerPage(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) queryMetrics(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	from, to, ok := parseRange(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_range", "from/to must be UTC date-times within 31 days", false)
		return
	}
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	resolution := r.URL.Query().Get("resolution")
	if resolution != "" && resolution != "raw" && resolution != "minute" && resolution != "hour" {
		writeAPIError(w, http.StatusBadRequest, "invalid_resolution", "resolution must be raw, minute, or hour", false)
		return
	}
	if resolution == "minute" || resolution == "hour" {
		seconds := 60
		if resolution == "hour" {
			seconds = 3600
		}
		rollups, nextCursor, truncated, err := a.Store.QueryRollups(r.Context(), serverID, from, to, seconds, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			if errors.Is(err, ErrRollupsPending) {
				writeAPIError(w, http.StatusServiceUnavailable, "rollups_pending", "metric summaries are temporarily unavailable; retry later", true)
				return
			}
			writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
			return
		}
		page := MetricPage{Samples: make([]contracts.MetricSample, 0), Rollups: rollups, Coverage: CoverageForSamples(nil, rollups), NextCursor: nextCursor, Truncated: truncated}
		writeJSON(w, http.StatusOK, page)
		return
	}
	page, err := a.Store.QueryMetricsWithGapCursor(r.Context(), serverID, from, to, limit, r.URL.Query().Get("cursor"), r.URL.Query().Get("gaps_cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	if page.Samples == nil {
		page.Samples = make([]contracts.MetricSample, 0)
	}
	if page.Rollups == nil {
		page.Rollups = make([]Rollup, 0)
	}
	page.Coverage = CoverageForSamples(page.Samples, page.Rollups, page.Gaps)
	writeJSON(w, http.StatusOK, page)
}

func (a *API) queryTraffic(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	from, to, ok := parseRange(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_range", "from/to must be UTC date-times within 31 days", false)
		return
	}
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	page, err := a.Store.QueryTrafficPeriods(r.Context(), serverID, from, to, r.URL.Query().Get("scope"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) listLogSources(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	page, err := a.Store.QueryLogSourcePage(r.Context(), serverID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	for i := range page.Items {
		page.Items[i].Path = ""
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) queryLogs(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	sourceID := r.URL.Query().Get("source")
	if sourceID == "" || len(sourceID) > 128 {
		writeAPIError(w, http.StatusBadRequest, "invalid_source", "source is required", false)
		return
	}
	from, to, ok := parseRange(r)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_range", "from/to must be UTC date-times within 31 days", false)
		return
	}
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	severity := r.URL.Query().Get("severity")
	search := r.URL.Query().Get("search")
	if len(severity) > 32 || len(search) > 256 {
		writeAPIError(w, http.StatusBadRequest, "invalid_filter", "severity/search exceeds the permitted bound", false)
		return
	}
	source, found, sourceErr := a.Store.GetLogSource(r.Context(), serverID, sourceID)
	if sourceErr != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read log sources", true)
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "log_source_not_found", "configured log source was not found", false)
		return
	}
	// Historical queries are bounded snapshots of the registered source. Persist
	// each snapshot before querying so cursors and duplicate suppression remain
	// stable across requests.
	sourceCursor := ""
	journalCursor := ""
	if rawCursor := r.URL.Query().Get("cursor"); rawCursor != "" {
		decodedCursor, cursorErr := decodeLogCursor(rawCursor)
		if cursorErr != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid", false)
			return
		}
		if source.Path == "" {
			journalCursor = decodedCursor.Cursor
		} else {
			sourceCursor = decodedCursor.Cursor
		}
	}
	if source.Path != "" {
		snapshot, readErr := ReadConfiguredLogCursor(r.Context(), source, LogReadOptions{MaxEntries: MaxPageItems, MaxBytes: MaxLogBytes, MaxLineSize: DefaultLogLineSize, From: from, To: to, Severity: severity, Search: search}, sourceCursor)
		if readErr != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "log_source_unavailable", "configured log source could not be read", true)
			return
		}
		if len(snapshot.Entries) > 0 {
			if _, insertErr := a.Store.InsertLogEntries(r.Context(), snapshot.Entries); insertErr != nil {
				writeAPIError(w, http.StatusServiceUnavailable, "log_snapshot_unavailable", "log snapshot could not be stored", true)
				return
			}
		}
	} else {
		// An empty path denotes a registered journald unit. The source ID is
		// already a bounded identifier and is passed as a fixed journalctl -u
		// argument by ReadJournal; no shell command or arbitrary path is accepted.
		snapshot, readErr := ReadJournal(r.Context(), source, JournalOptions{Unit: source.ID, Cursor: journalCursor, MaxEntries: MaxPageItems, MaxBytes: MaxLogBytes, Since: from, Until: to})
		if readErr != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "journal_unavailable", "the configured journal source could not be read", true)
			return
		}
		filtered := make([]LogEntry, 0, len(snapshot.Entries))
		options := LogReadOptions{From: from, To: to, Severity: severity, Search: search}
		for _, entry := range snapshot.Entries {
			if includeLog(entry, options) {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) > 0 {
			if _, insertErr := a.Store.InsertLogEntries(r.Context(), filtered); insertErr != nil {
				writeAPIError(w, http.StatusServiceUnavailable, "log_snapshot_unavailable", "journal snapshot could not be stored", true)
				return
			}
		}
	}
	order := strings.ToLower(r.URL.Query().Get("order"))
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		writeAPIError(w, http.StatusBadRequest, "invalid_order", "order must be asc or desc", false)
		return
	}
	page, err := a.Store.QueryLogsFilteredOrdered(r.Context(), serverID, sourceID, from, to, severity, search, limit, r.URL.Query().Get("cursor"), order)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) tailLogs(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	if sourceID := r.URL.Query().Get("source"); sourceID == "" || len(sourceID) > 128 || !isSafeIdentifier(sourceID) {
		writeAPIError(w, http.StatusBadRequest, "invalid_source", "source is required", false)
		return
	} else {
		maxEvents, err := parseBoundedInt(r.URL.Query().Get("max_events"), 25, 1, 100)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_max_events", err.Error(), false)
			return
		}
		maxBytes, err := parseBoundedInt(r.URL.Query().Get("max_bytes"), 262144, 1024, MaxLogBytes)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_max_bytes", err.Error(), false)
			return
		}
		maxDuration, err := parseBoundedInt(r.URL.Query().Get("max_duration_seconds"), 30, 1, int(MaxLiveTailDuration/time.Second))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_max_duration", err.Error(), false)
			return
		}
		// The source path is intentionally never accepted from the request. It
		// must be registered in the local store before a live tail is allowed.
		source, found, sourceErr := a.Store.GetLogSource(r.Context(), serverID, sourceID)
		if sourceErr != nil {
			writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read log sources", true)
			return
		}
		if !found {
			writeAPIError(w, http.StatusNotFound, "log_source_not_found", "configured log source was not found", false)
			return
		}
		var result LogReadResult
		if source.Path == "" {
			result, err = TailJournal(r.Context(), source, JournalTailOptions{Cursor: r.URL.Query().Get("cursor"), MaxEntries: maxEvents, MaxBytes: maxBytes, MaxDuration: time.Duration(maxDuration) * time.Second})
		} else {
			result, err = TailConfiguredLog(r.Context(), source, LogTailOptions{Cursor: r.URL.Query().Get("cursor"), MaxEntries: maxEvents, MaxBytes: maxBytes, MaxDuration: time.Duration(maxDuration) * time.Second})
		}
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "tail_failed", err.Error(), false)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		events := make([][]byte, 0, len(result.Entries))
		responseBytes := 0
		truncated := result.Truncated
		for index, entry := range result.Entries {
			sequence := uint64(index)
			if _, offset, _, cursorErr := decodeFileCursorWithFingerprint(entry.Cursor); cursorErr == nil && offset >= 0 {
				sequence = uint64(offset)
			}
			data, marshalErr := json.Marshal(map[string]any{"entry": entry, "sequence": strconv.FormatUint(sequence, 10), "dropped_events": 0})
			if marshalErr != nil || responseBytes+len(data)+1 > contracts.MaxEnvelopeBytes {
				truncated = true
				break
			}
			events = append(events, append(data, '\n'))
			responseBytes += len(data) + 1
		}
		if truncated {
			w.Header().Set("X-Payesh-Truncated", "true")
		}
		for _, data := range events {
			if _, err := w.Write(data); err != nil {
				return
			}
		}
	}
}

func parseBoundedInt(value string, defaultValue, minimum, maximum int) (int, error) {
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, errors.New("value is outside the permitted bound")
	}
	return parsed, nil
}

func parseLimit(value string) (int, bool) {
	if value == "" {
		return MaxPageItems, true
	}
	limit, err := strconv.Atoi(value)
	return limit, err == nil && limit >= 1 && limit <= MaxPageItems
}

func parseRange(r *http.Request) (time.Time, time.Time, bool) {
	var from, to time.Time
	var err error
	fromValue, toValue := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if fromValue == "" || toValue == "" {
		return time.Time{}, time.Time{}, false
	}
	from, err = time.Parse(time.RFC3339Nano, fromValue)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	to, err = time.Parse(time.RFC3339Nano, toValue)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	if !from.IsZero() && !to.IsZero() {
		if to.Before(from) || to.Sub(from) > 31*24*time.Hour {
			return time.Time{}, time.Time{}, false
		}
	}
	return from, to, true
}

func CoverageForSamples(samples []contracts.MetricSample, rollups []Rollup, gapPages ...[]contracts.CoverageGap) map[string]float64 {
	coverage := map[string]float64{}
	if len(rollups) > 0 {
		for _, rollup := range rollups {
			current, exists := coverage[rollup.Metric]
			if !exists || current > 0.5 && rollup.Coverage != "complete" {
				if rollup.Coverage == "complete" {
					coverage[rollup.Metric] = 1
				} else {
					coverage[rollup.Metric] = 0.5
				}
			}
		}
		return coverage
	}
	counts := map[string][2]int{}
	for _, sample := range samples {
		metrics := make(map[string]struct{}, len(sample.Values)+len(sample.Counters)+len(sample.Validity))
		for metric := range sample.Values {
			metrics[metric] = struct{}{}
		}
		for metric := range sample.Counters {
			metrics[metric] = struct{}{}
		}
		for metric := range sample.Validity {
			metrics[metric] = struct{}{}
		}
		for metric := range metrics {
			counts[metric] = [2]int{counts[metric][0] + 1, counts[metric][1] + 1}
			if state := sample.Validity[metric]; state != "" && state != "valid" {
				pair := counts[metric]
				pair[1]--
				counts[metric] = pair
			}
		}
	}
	for metric, pair := range counts {
		if pair[0] > 0 {
			coverage[metric] = float64(pair[1]) / float64(pair[0])
		}
	}
	if len(gapPages) > 0 && len(gapPages[0]) > 0 {
		// A sequence gap affects every metric in the affected sample stream;
		// expose that uncertainty instead of reporting a misleading 100% raw
		// coverage ratio. The detailed gap records remain available separately.
		for metric, value := range coverage {
			if value > 0.5 {
				coverage[metric] = 0.5
			}
		}
	}
	return coverage
}

func writeAPIError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(w, status, contracts.Error{Code: code, Message: message, Retryable: retryable})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"serialization_error","message":"response could not be encoded","retryable":false}`))
		return
	}
	if len(data)+1 > contracts.MaxEnvelopeBytes {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"code":"response_too_large","message":"response exceeds the 1 MiB bound","retryable":false}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
