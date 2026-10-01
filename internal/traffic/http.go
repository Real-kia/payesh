package traffic

// The traffic HTTP adapter is intentionally small: it exposes the durable
// period/allowance semantics without turning the package into a generic query
// language or silently authorizing a bandwidth control policy.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const maxRequestBytes = 1 << 20

var trafficServerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var trafficScopePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

var (
	errConfigurationRevisionConflict = errors.New("configuration_revision_conflict")
	errTrafficIdempotencyConflict    = errors.New("traffic_idempotency_conflict")
)

type Service struct {
	Store   *monitoring.Store
	Manager *Manager
	Alerts  *alerts.Engine
	Now     func() time.Time
	mu      sync.Mutex
}

func NewService(store *monitoring.Store) (*Service, error) {
	manager, err := NewManager(store)
	if err != nil {
		return nil, err
	}
	alertEngine, err := alerts.NewEngine(store)
	if err != nil {
		return nil, err
	}
	return &Service{Store: store, Manager: manager, Alerts: alertEngine, Now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/servers/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeTrafficError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "traffic" || !trafficServerIDPattern.MatchString(parts[0]) {
		writeTrafficError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	serverID := contracts.ServerID(parts[0])
	if len(parts) == 3 {
		if parts[2] == "usage" && r.Method == http.MethodGet {
			s.rangeUsage(w, r, serverID)
			return
		}
		if parts[2] != "forecast" || r.Method != http.MethodGet {
			writeTrafficError(w, http.StatusNotFound, "not_found", "resource not found", false)
			return
		}
		s.forecast(w, r, serverID)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.query(w, r, serverID)
	case http.MethodPost:
		s.configure(w, r, serverID)
	default:
		writeTrafficError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported for traffic", false)
	}
}

func (s *Service) rangeUsage(w http.ResponseWriter, r *http.Request, id contracts.ServerID) {
	from, fromErr := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, toErr := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if fromErr != nil || toErr != nil || from.IsZero() || !to.After(from) || to.Sub(from) > 90*24*time.Hour || !from.Equal(from.Truncate(time.Hour)) || !to.Equal(to.Truncate(time.Hour)) || to.After(s.Now().UTC().Truncate(time.Hour)) {
		writeTrafficError(w, http.StatusBadRequest, "invalid_range", "choose completed whole UTC hours spanning at most 90 days", false)
		return
	}
	result, err := s.Store.QueryTrafficRange(r.Context(), id, from, to)
	if errors.Is(err, monitoring.ErrRollupsPending) {
		writeTrafficError(w, http.StatusServiceUnavailable, "history_pending", "traffic history is being rebuilt; retry shortly", true)
		return
	}
	if err != nil {
		writeTrafficError(w, http.StatusServiceUnavailable, "history_unavailable", "traffic history could not be loaded; retry shortly", true)
		return
	}
	writeTrafficJSON(w, http.StatusOK, result)
}

func (s *Service) forecast(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	scope := r.URL.Query().Get("scope")
	direction := r.URL.Query().Get("direction")
	if scope == "" || !trafficScopePattern.MatchString(scope) || (direction != "inbound" && direction != "outbound" && direction != "combined") {
		writeTrafficError(w, http.StatusBadRequest, "invalid_forecast_identity", "scope and direction are required", false)
		return
	}
	asOf := time.Time{}
	var err error
	asOfProvided := r.URL.Query().Get("as_of") != ""
	if value := r.URL.Query().Get("as_of"); value != "" {
		asOf, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			writeTrafficError(w, http.StatusBadRequest, "invalid_forecast_time", "as_of must be an RFC3339 timestamp", false)
			return
		}
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if asOf.IsZero() {
		asOf = now
	} else if asOf.UTC().After(now) {
		writeTrafficError(w, http.StatusBadRequest, "invalid_forecast_time", "as_of cannot be in the future", false)
		return
	}
	allowance, found, err := s.Store.GetTrafficAllowance(r.Context(), serverID, scope, direction)
	if err != nil {
		writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read traffic allowance", true)
		return
	}
	if !found {
		writeTrafficError(w, http.StatusNotFound, "traffic_allowance_not_found", "traffic allowance was not found", false)
		return
	}
	period, periodFound, err := s.Store.GetActiveTrafficPeriod(r.Context(), serverID, scope, direction, asOf)
	if err != nil {
		writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read active traffic period", true)
		return
	}
	if !periodFound {
		period, err = PeriodFor(asOf, allowance)
		if err != nil {
			writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not derive traffic period", true)
			return
		}
	}
	windowEnd := asOf.UTC()
	if period.To.UTC().Before(windowEnd) {
		windowEnd = period.To.UTC()
	}
	var forecast contracts.TrafficForecast
	if !windowEnd.After(period.From.UTC()) {
		zero := uint64(0)
		forecast, err = forecastWithObservedBytes(period, nil, asOf, &zero)
		forecast.Available = false
		forecast.Label = ""
		forecast.Reason = "as_of_counter_unavailable"
	} else {
		// Read the minimum useful window first. A 5-second stream contains more
		// than 50,000 samples in seven days, so replaying the whole billing
		// period (or always reading seven days) would make an otherwise healthy
		// forecast unavailable. Sparse streams fall back to the documented
		// seven-day window when a 24-hour window cannot establish coverage.
		loadWindow := func(window time.Duration) (contracts.TrafficForecast, []contracts.TrafficObservation, bool, bool, error) {
			recentStart := asOf.UTC().Add(-window)
			if period.From.UTC().After(recentStart) {
				recentStart = period.From.UTC()
			}
			// Include one maximum valid counter interval before the recent
			// window. This lets a normal periodic stream establish coverage at
			// the exact 24-hour edge even when no point lands on that nanosecond.
			historyStart := recentStart.Add(-MaxForecastInterval)
			if period.From.UTC().After(historyStart) {
				historyStart = period.From.UTC()
			}
			samples, samplesTruncated, loadErr := s.Store.QueryMetricSamplesRange(r.Context(), serverID, historyStart, windowEnd, monitoring.MaxForecastSamples)
			if loadErr != nil {
				return contracts.TrafficForecast{}, nil, false, false, loadErr
			}
			periodPage, periodsErr := s.Store.QueryTrafficPeriodsByDirection(r.Context(), serverID, historyStart, windowEnd, scope, direction, monitoring.MaxPageItems, "")
			if periodsErr != nil {
				return contracts.TrafficForecast{}, nil, false, false, periodsErr
			}
			periods := periodPage.Periods
			periodSnapshotFound := false
			for _, historical := range periods {
				if historical.Scope == period.Scope && historical.Direction == period.Direction && historical.From.UTC().Equal(period.From.UTC()) {
					periodSnapshotFound = true
					break
				}
			}
			if !periodSnapshotFound {
				periods = append(periods, period)
			}

			// If the period begins inside the selected window it is already present.
			// Otherwise fetch only an exact boundary sample, never an unbounded
			// period replay. That sample can provide an exact as-of total when the
			// retained series is contiguous; if it cannot, the handler fails closed
			// for historical as_of requests instead of reporting a lower bound as an
			// authoritative billing total.
			if period.From.UTC().Before(historyStart.UTC()) {
				baseline, baselineTruncated, baselineErr := s.Store.QueryMetricSamplesRange(r.Context(), serverID, period.From.UTC(), period.From.UTC(), 1)
				if baselineErr != nil {
					return contracts.TrafficForecast{}, nil, false, false, baselineErr
				}
				samplesTruncated = samplesTruncated || baselineTruncated
				for _, candidate := range baseline {
					duplicate := false
					for _, existing := range samples {
						if existing.CollectorEpoch == candidate.CollectorEpoch && existing.Sequence == candidate.Sequence {
							duplicate = true
							break
						}
					}
					if !duplicate {
						samples = append(samples, candidate)
					}
				}
			}
			observations := trafficObservationsForPeriods(samples, periods)
			observed := period.CountedBytes
			countedKnown := true
			// A durable total is authoritative for the live request and for a
			// completed period. For an explicit historical as_of inside an active
			// period, use only a contiguous boundary-to-as_of replay; never expose
			// the eventual period total, which may include future samples.
			if asOf.UTC().Before(period.To.UTC()) && asOfProvided {
				var counted bool
				observed, counted = observedBytesAtAsOf(period, observations, asOf)
				countedKnown = counted
			}
			observedForForecast := observed
			result, err := forecastWithObservedBytesWindow(period, observations, asOf, &observedForForecast, &recentStart)
			if !countedKnown {
				result.Available = false
				result.Label = ""
				result.Reason = "as_of_counter_unavailable"
			}
			if periodPage.Truncated || (samplesTruncated && !result.Available) {
				result.Available = false
				result.Label = ""
				result.Reason = "forecast_sample_limit_exceeded"
			}
			return result, observations, samplesTruncated, periodPage.Truncated, err
		}

		var samplesTruncated, periodsTruncated bool
		forecast, _, samplesTruncated, periodsTruncated, err = loadWindow(24 * time.Hour)
		if err == nil && !forecast.Available && !samplesTruncated && !periodsTruncated {
			forecast, _, samplesTruncated, periodsTruncated, err = loadWindow(RecentForecast)
		}
	}
	if err != nil {
		writeTrafficError(w, http.StatusServiceUnavailable, "forecast_unavailable", "traffic forecast could not be computed", true)
		return
	}
	writeTrafficJSON(w, http.StatusOK, forecast)
}

func (s *Service) query(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	from, to, err := parseTrafficRange(r)
	if err != nil {
		writeTrafficError(w, http.StatusBadRequest, "invalid_range", err.Error(), false)
		return
	}
	limit, err := parseTrafficLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeTrafficError(w, http.StatusBadRequest, "invalid_limit", err.Error(), false)
		return
	}
	page, err := s.Store.QueryTrafficPeriods(r.Context(), serverID, from, to, r.URL.Query().Get("scope"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeTrafficError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeTrafficJSON(w, http.StatusOK, page)
}

type allowanceRequest struct {
	IdempotencyKey     string   `json:"idempotency_key"`
	ExpectedRevision   uint64   `json:"expected_revision,string"`
	Scope              string   `json:"scope"`
	Interfaces         []string `json:"interfaces,omitempty"`
	AllowanceBytes     uint64   `json:"allowance_bytes,string"`
	ResetDay           uint8    `json:"reset_day"`
	Timezone           string   `json:"timezone"`
	Direction          string   `json:"direction"`
	WarningPercentages []uint8  `json:"warning_percentages,omitempty"`
	StartNewPeriod     bool     `json:"start_new_period,omitempty"`
}

type allowanceResult struct {
	Allowance             contracts.TrafficAllowance `json:"allowance"`
	Period                contracts.TrafficPeriod    `json:"period"`
	Preview               *PeriodPreview             `json:"preview,omitempty"`
	ConfigurationRevision uint64                     `json:"configuration_revision,string"`
}

func (s *Service) configure(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var request allowanceRequest
	if !decodeTrafficRequest(w, r, &request) {
		return
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		writeTrafficError(w, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1..128 characters", false)
		return
	}
	hash := hashAllowanceRequest(request)
	if record, found, err := s.Store.GetTrafficAllowanceRequest(r.Context(), serverID, request.IdempotencyKey); err != nil {
		writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read traffic request history", true)
		return
	} else if found {
		if record.RequestHash != hash {
			writeTrafficError(w, http.StatusConflict, "idempotency_conflict", "idempotency_key was already used for a different traffic configuration", false)
			return
		}
		var result allowanceResult
		if err := json.Unmarshal(record.ResultJSON, &result); err != nil {
			writeTrafficError(w, http.StatusInternalServerError, "storage_error", "stored traffic result is invalid", true)
			return
		}
		writeTrafficJSON(w, http.StatusOK, result)
		return
	}
	server, found, err := s.Store.GetServer(r.Context(), serverID)
	if err != nil {
		writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read server", true)
		return
	}
	if !found {
		writeTrafficError(w, http.StatusNotFound, "not_found", "server not found", false)
		return
	}
	if server.ConfigurationRevision != request.ExpectedRevision {
		writeTrafficError(w, http.StatusConflict, "configuration_revision_conflict", "server configuration changed; reload before applying traffic settings", false)
		return
	}
	allowance := WithDefaults(contracts.TrafficAllowance{Scope: request.Scope, Interfaces: request.Interfaces, AllowanceBytes: request.AllowanceBytes, ResetDay: request.ResetDay, Timezone: request.Timezone, Direction: request.Direction, WarningPercentages: request.WarningPercentages})
	if err := allowance.Validate(); err != nil {
		writeTrafficError(w, http.StatusBadRequest, "invalid_traffic_allowance", err.Error(), false)
		return
	}
	previous, previousFound, previousErr := s.Store.GetTrafficAllowance(r.Context(), serverID, allowance.Scope, allowance.Direction)
	if previousErr != nil {
		writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read current traffic allowance", true)
		return
	}
	at := time.Now().UTC()
	if s.Now != nil {
		at = s.Now().UTC()
	}
	var preview *PeriodPreview
	if previousFound {
		current, currentFound, currentErr := s.Store.GetActiveTrafficPeriod(r.Context(), serverID, previous.Scope, previous.Direction, at)
		if currentErr != nil {
			writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read current traffic period", true)
			return
		}
		if !currentFound {
			current, currentErr = PeriodFor(at, previous)
			if currentErr != nil {
				writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not derive current traffic period", true)
				return
			}
		}
		// The durable allowance row already contains the proposed interface set
		// while a schedule change is pending. Preview the active period using the
		// previous set until its effective boundary, otherwise a second edit can
		// falsely report an interface change that is not active yet.
		if pending, pendingFound, pendingErr := s.Store.GetTrafficAllowanceChange(r.Context(), serverID, previous.Scope, previous.Direction); pendingErr != nil {
			writeTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read pending traffic schedule", true)
			return
		} else if pendingFound && at.Before(pending.EffectiveAt) {
			current.Interfaces = append([]string(nil), pending.Previous.Interfaces...)
		}
		if proposedPreview, previewErr := PreviewChange(current, allowance, at, request.StartNewPeriod); previewErr == nil {
			preview = &proposedPreview
		}
	}
	var result allowanceResult
	var replayJSON []byte
	err = s.Store.WithTransaction(r.Context(), func(tx *sql.Tx) error {
		// Re-read idempotency state inside the transaction. The first read above
		// is only a fast path; this one closes the cross-process race.
		var existingHash, existingJSON string
		readErr := tx.QueryRowContext(r.Context(), `SELECT request_hash,result_json FROM traffic_allowance_requests WHERE server_id=? AND idempotency_key=?`, string(serverID), request.IdempotencyKey).Scan(&existingHash, &existingJSON)
		if readErr == nil {
			if existingHash != hash {
				return errTrafficIdempotencyConflict
			}
			replayJSON = []byte(existingJSON)
			return nil
		}
		if !errors.Is(readErr, sql.ErrNoRows) {
			return readErr
		}
		if request.ExpectedRevision == ^uint64(0) {
			return errors.New("configuration revision is exhausted")
		}
		nextRevision := request.ExpectedRevision + 1
		changed, updateErr := tx.ExecContext(r.Context(), `UPDATE servers SET configuration_revision=? WHERE id=? AND configuration_revision=?`, strconv.FormatUint(nextRevision, 10), string(serverID), strconv.FormatUint(request.ExpectedRevision, 10))
		if updateErr != nil {
			return updateErr
		}
		if rows, rowsErr := changed.RowsAffected(); rowsErr != nil {
			return rowsErr
		} else if rows != 1 {
			return errConfigurationRevisionConflict
		}
		if s.Alerts != nil {
			if err := s.Alerts.EnsureTrafficRulesTx(r.Context(), tx, serverID, allowance); err != nil {
				return err
			}
		}
		period, applyErr := s.Manager.applyAllowanceTx(r.Context(), tx, serverID, allowance, at, 0, "complete", request.StartNewPeriod, true)
		if applyErr != nil {
			return applyErr
		}
		result = allowanceResult{Allowance: allowance, Period: period, Preview: preview, ConfigurationRevision: nextRevision}
		if result.Preview == nil {
			if proposedPreview, previewErr := PreviewChange(period, allowance, at, request.StartNewPeriod); previewErr == nil {
				result.Preview = &proposedPreview
			}
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return marshalErr
		}
		_, insertErr := tx.ExecContext(r.Context(), `INSERT INTO traffic_allowance_requests(server_id,idempotency_key,request_hash,result_json,created_at) VALUES(?,?,?,?,?)`, string(serverID), request.IdempotencyKey, hash, string(encoded), monitoring.FormatPersistedTime(time.Now()))
		return insertErr
	})
	if err != nil {
		switch {
		case errors.Is(err, errTrafficIdempotencyConflict):
			writeTrafficError(w, http.StatusConflict, "idempotency_conflict", "idempotency_key was already used for a different traffic configuration", false)
		case errors.Is(err, errConfigurationRevisionConflict):
			writeTrafficError(w, http.StatusConflict, "configuration_revision_conflict", "server configuration changed; reload before applying traffic settings", false)
		case errors.Is(err, sql.ErrNoRows):
			writeTrafficError(w, http.StatusNotFound, "not_found", "server not found", false)
		default:
			writeTrafficError(w, http.StatusBadRequest, "traffic_configuration_failed", err.Error(), false)
		}
		return
	}
	if len(replayJSON) > 0 {
		if err := json.Unmarshal(replayJSON, &result); err != nil {
			writeTrafficError(w, http.StatusInternalServerError, "storage_error", "stored traffic result is invalid", true)
			return
		}
	}
	writeTrafficJSON(w, http.StatusOK, result)
}

func hashAllowanceRequest(request allowanceRequest) string {
	request = canonicalAllowanceRequest(request)
	encoded, _ := json.Marshal(request)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func canonicalAllowanceRequest(request allowanceRequest) allowanceRequest {
	if request.ResetDay == 0 {
		request.ResetDay = DefaultResetDay
	}
	if request.Timezone == "" {
		request.Timezone = "UTC"
	}
	if len(request.WarningPercentages) == 0 {
		request.WarningPercentages = []uint8{80, 90, 100}
	}
	request.Interfaces = append([]string(nil), request.Interfaces...)
	return request
}

func decodeTrafficRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeTrafficError(w, http.StatusBadRequest, "invalid_request", "request body is invalid", false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeTrafficError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value", false)
		return false
	}
	return true
}

func parseTrafficRange(r *http.Request) (time.Time, time.Time, error) {
	var from, to time.Time
	var err error
	if value := r.URL.Query().Get("from"); value != "" {
		from, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from must be an RFC3339 timestamp")
		}
	}
	if value := r.URL.Query().Get("to"); value != "" {
		to, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to must be an RFC3339 timestamp")
		}
	}
	if !from.IsZero() && !to.IsZero() {
		if !to.After(from) {
			return time.Time{}, time.Time{}, errors.New("to must be after from")
		}
		if to.Sub(from) > 31*24*time.Hour {
			return time.Time{}, time.Time{}, errors.New("range exceeds 31 days")
		}
	}
	return from, to, nil
}

func parseTrafficLimit(value string) (int, error) {
	if value == "" {
		return monitoring.MaxPageItems, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > monitoring.MaxPageItems {
		return 0, errors.New("limit must be 1..200")
	}
	return limit, nil
}

func writeTrafficError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeTrafficJSON(w, status, contracts.Error{Code: code, Message: message, Retryable: retryable})
}

func writeTrafficJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > contracts.MaxEnvelopeBytes {
		data = []byte(`{"code":"response_too_large","message":"response exceeds the size limit","retryable":false}`)
		status = http.StatusRequestEntityTooLarge
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
