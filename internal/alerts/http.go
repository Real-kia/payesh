package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const maxRequestBytes = 1 << 20

type Service struct {
	Store  *monitoring.Store
	Engine *Engine
	mu     sync.Mutex
}

func NewService(store *monitoring.Store) (*Service, error) {
	engine, err := NewEngine(store)
	if err != nil {
		return nil, err
	}
	return &Service{Store: store, Engine: engine}, nil
}

// ConfigureDelivery supplies owner-configured destinations to the engine
// without storing their credentials in the browser-facing database.
func (s *Service) ConfigureDelivery(queue *DeliveryQueue, destinations func(context.Context, contracts.ServerID) []NotificationDestination) {
	if s == nil || s.Engine == nil {
		return
	}
	s.Engine.ConfigureDelivery(queue, destinations)
}

// EnsureStarterRulesForServers installs the editable, enabled defaults for
// every currently known server. IDs and idempotency keys are deterministic, so
// running this during bootstrap or after a restart is safe and does not reset
// owner edits to other rules.
func (s *Service) EnsureStarterRulesForServers(ctx context.Context) error {
	cursor := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		page, err := s.Store.QueryServerPage(ctx, monitoring.MaxPageItems, cursor)
		if err != nil {
			return err
		}
		for _, server := range page.Items {
			if err := s.Engine.EnsureStarterRules(ctx, server.ID); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
	return errors.New("server count exceeds starter-rule initialization bound")
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/alerts/rules" && r.Method == http.MethodGet:
		s.listRules(w, r)
	case r.URL.Path == "/api/v1/alerts/rules" && r.Method == http.MethodPost:
		s.createRule(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/alerts/rules/") && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		s.updateRule(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/alerts/rules/"))
	case r.URL.Path == "/api/v1/alerts" && r.Method == http.MethodGet:
		s.listStates(w, r)
	case r.URL.Path == "/api/v1/alerts/history" && r.Method == http.MethodGet:
		s.history(w, r)
	case r.URL.Path == "/api/v1/maintenance-windows" && r.Method == http.MethodGet:
		s.listMaintenance(w, r)
	case r.URL.Path == "/api/v1/maintenance-windows" && r.Method == http.MethodPost:
		s.createMaintenance(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/incidents/") && r.Method == http.MethodGet:
		s.incident(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/incidents/"))
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found", false)
	}
}

type alertRuleRequest struct {
	Name              string             `json:"name"`
	Expression        string             `json:"expression"`
	IdempotencyKey    string             `json:"idempotency_key"`
	ServerID          contracts.ServerID `json:"server_id,omitempty"`
	Enabled           *bool              `json:"enabled,omitempty"`
	DurationSeconds   int64              `json:"duration_seconds,omitempty"`
	RecoveryThreshold *float64           `json:"recovery_threshold,omitempty"`
	ReminderSeconds   *int64             `json:"reminder_seconds,omitempty"`
	GroupKey          string             `json:"group_key,omitempty"`
}

// alertRuleUpdateRequest deliberately uses pointers so PATCH can change a
// boolean to false or a numeric threshold to zero without conflating an
// omitted field with its zero value. The creation idempotency key is stable;
// if supplied on update it must match the original rule.
type alertRuleUpdateRequest struct {
	Name              *string  `json:"name,omitempty"`
	Expression        *string  `json:"expression,omitempty"`
	IdempotencyKey    string   `json:"idempotency_key,omitempty"`
	Enabled           *bool    `json:"enabled,omitempty"`
	DurationSeconds   *int64   `json:"duration_seconds,omitempty"`
	RecoveryThreshold *float64 `json:"recovery_threshold,omitempty"`
	ReminderSeconds   *int64   `json:"reminder_seconds,omitempty"`
	GroupKey          *string  `json:"group_key,omitempty"`
}

type maintenanceRequest struct {
	IdempotencyKey string               `json:"idempotency_key"`
	StartsAt       time.Time            `json:"starts_at"`
	EndsAt         time.Time            `json:"ends_at"`
	ServerIDs      []contracts.ServerID `json:"server_ids,omitempty"`
	Reason         string               `json:"reason,omitempty"`
}

func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request body is invalid", false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value", false)
		return false
	}
	return true
}

func (s *Service) createRule(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var request alertRuleRequest
	if !decodeRequest(w, r, &request) {
		return
	}
	if request.Name == "" || request.Expression == "" || request.IdempotencyKey == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_rule", "name, expression, and idempotency_key are required", false)
		return
	}
	metric, _, _, err := ParseExpression(request.Expression)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_expression", err.Error(), false)
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	group := request.GroupKey
	if group == "" {
		group = metric
	}
	duration := request.DurationSeconds
	if duration == 0 {
		duration = int64(DefaultAlertDuration / time.Second)
	}
	reminder := int64(DefaultReminder / time.Second)
	if request.ReminderSeconds != nil {
		reminder = *request.ReminderSeconds
	}
	rule := contracts.AlertRule{ID: stableID("rule", request.IdempotencyKey), Name: request.Name, Expression: request.Expression, ServerID: request.ServerID, Enabled: enabled, DurationSeconds: duration, RecoveryThreshold: request.RecoveryThreshold, ReminderSeconds: reminder, GroupKey: group, IdempotencyKey: request.IdempotencyKey, CreatedAt: time.Now().UTC()}
	if existing, found, err := s.Store.GetAlertRule(r.Context(), rule.ID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read alert idempotency state", true)
		return
	} else if found {
		if !sameAlertRuleRequest(existing, rule) {
			writeAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency_key was already used for a different alert rule", false)
			return
		}
		writeJSON(w, http.StatusCreated, existing)
		return
	}
	if err := s.Engine.AddRule(r.Context(), rule); err != nil {
		if errors.Is(err, monitoring.ErrAlertRuleLimit) {
			writeAPIError(w, http.StatusConflict, "alert_rule_limit_reached", err.Error(), false)
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_rule", err.Error(), false)
		return
	}
	persisted, found, err := s.Store.GetAlertRule(r.Context(), rule.ID)
	if err != nil || !found {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not reload created alert rule", true)
		return
	}
	writeJSON(w, http.StatusCreated, persisted)
}

func (s *Service) updateRule(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || len(id) > 128 || !safeRuleIdentifier(id) {
		writeAPIError(w, http.StatusNotFound, "not_found", "alert rule not found", false)
		return
	}
	rule, found, err := s.Store.GetAlertRule(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read alert rule", true)
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "alert rule not found", false)
		return
	}
	var request alertRuleUpdateRequest
	if !decodeRequest(w, r, &request) {
		return
	}
	if request.IdempotencyKey != "" && request.IdempotencyKey != rule.IdempotencyKey {
		writeAPIError(w, http.StatusConflict, "idempotency_conflict", "alert rule idempotency key cannot change", false)
		return
	}
	if request.Name == nil && request.Expression == nil && request.Enabled == nil && request.DurationSeconds == nil && request.RecoveryThreshold == nil && request.ReminderSeconds == nil && request.GroupKey == nil && request.IdempotencyKey == "" {
		message := "alert rule update must include at least one field"
		if r.Method == http.MethodPut {
			message = "PUT must include alert rule fields"
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_rule", message, false)
		return
	}
	if request.Name != nil {
		rule.Name = *request.Name
	}
	if request.Expression != nil {
		rule.Expression = *request.Expression
	}
	if request.Enabled != nil {
		rule.Enabled = *request.Enabled
	}
	if request.DurationSeconds != nil {
		rule.DurationSeconds = *request.DurationSeconds
	}
	if request.RecoveryThreshold != nil {
		value := *request.RecoveryThreshold
		rule.RecoveryThreshold = &value
	}
	if request.ReminderSeconds != nil {
		rule.ReminderSeconds = *request.ReminderSeconds
	}
	if request.GroupKey != nil {
		rule.GroupKey = *request.GroupKey
	}
	// Any explicit owner PATCH supersedes an internal generated-rule disable
	// marker; otherwise a later allowance reconciliation could resurrect the
	// owner's deliberate disable.
	rule.DisableReason = ""
	if _, _, _, parseErr := ParseExpression(rule.Expression); parseErr != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_expression", parseErr.Error(), false)
		return
	}
	if err := rule.Validate(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_rule", err.Error(), false)
		return
	}
	if err := s.Store.SaveAlertRule(r.Context(), rule); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_rule", err.Error(), false)
		return
	}
	persisted, found, err := s.Store.GetAlertRule(r.Context(), rule.ID)
	if err != nil || !found {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not reload updated alert rule", true)
		return
	}
	writeJSON(w, http.StatusOK, persisted)
}

func safeRuleIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' && character != '.' && character != ':' {
			return false
		}
	}
	return true
}

func sameAlertRuleRequest(left, right contracts.AlertRule) bool {
	if left.IdempotencyKey != right.IdempotencyKey || left.Name != right.Name || left.Expression != right.Expression || left.ServerID != right.ServerID || left.Enabled != right.Enabled || left.DurationSeconds != right.DurationSeconds || left.ReminderSeconds != right.ReminderSeconds || left.GroupKey != right.GroupKey {
		return false
	}
	if left.RecoveryThreshold == nil || right.RecoveryThreshold == nil {
		return left.RecoveryThreshold == nil && right.RecoveryThreshold == nil
	}
	return *left.RecoveryThreshold == *right.RecoveryThreshold
}

func (s *Service) listRules(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	page, err := s.Store.ListAlertRules(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Service) listStates(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	serverID := contracts.ServerID(r.URL.Query().Get("server_id"))
	page, err := s.Store.ListAlertStates(r.Context(), serverID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Service) history(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	from, to, err := parseOptionalRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_range", err.Error(), false)
		return
	}
	page, err := s.Store.ListAlertHistory(r.Context(), from, to, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Service) createMaintenance(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var request maintenanceRequest
	if !decodeRequest(w, r, &request) {
		return
	}
	window := contracts.MaintenanceWindow{ID: stableID("maintenance", request.IdempotencyKey), IdempotencyKey: request.IdempotencyKey, StartsAt: request.StartsAt, EndsAt: request.EndsAt, ServerIDs: request.ServerIDs, Reason: request.Reason}
	if existing, found, err := s.Store.GetMaintenanceWindowByIdempotency(r.Context(), window.IdempotencyKey); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read maintenance idempotency state", true)
		return
	} else if found {
		if existing.StartsAt.Equal(window.StartsAt) && existing.EndsAt.Equal(window.EndsAt) && existing.Reason == window.Reason && sameServerIDs(existing.ServerIDs, window.ServerIDs) {
			writeJSON(w, http.StatusCreated, existing)
			return
		}
		writeAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency_key was already used for a different maintenance window", false)
		return
	}
	if err := s.Store.SaveMaintenanceWindow(r.Context(), window); err != nil {
		if strings.Contains(err.Error(), "idempotency conflict") {
			writeAPIError(w, http.StatusConflict, "idempotency_conflict", err.Error(), false)
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_maintenance_window", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusCreated, window)
}

func sameServerIDs(left, right []contracts.ServerID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Service) listMaintenance(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(r.URL.Query().Get("limit"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200", false)
		return
	}
	page, err := s.Store.ListMaintenanceWindows(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_query", err.Error(), false)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Service) incident(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || len(id) > 128 {
		writeAPIError(w, http.StatusNotFound, "not_found", "incident not found", false)
		return
	}
	incident, found, err := s.Store.GetIncident(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "storage_error", "could not read incident", true)
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "not_found", "incident not found", false)
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func parseLimit(value string) (int, bool) {
	if value == "" {
		return monitoring.MaxPageItems, true
	}
	limit, err := strconv.Atoi(value)
	return limit, err == nil && limit >= 1 && limit <= monitoring.MaxPageItems
}

func parseOptionalRange(fromValue, toValue string) (time.Time, time.Time, error) {
	var from, to time.Time
	var err error
	if fromValue != "" {
		from, err = time.Parse(time.RFC3339Nano, fromValue)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from is not an RFC3339 timestamp")
		}
	}
	if toValue != "" {
		to, err = time.Parse(time.RFC3339Nano, toValue)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to is not an RFC3339 timestamp")
		}
	}
	if !from.IsZero() && !to.IsZero() && !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	if !from.IsZero() && !to.IsZero() && to.Sub(from) > 31*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("range exceeds 31 days")
	}
	return from, to, nil
}

func stableID(prefix, value string) string {
	// Deterministic IDs make retried POSTs idempotent without retaining the
	// caller's raw key in logs or an unbounded in-memory map.
	hash := sha256.Sum256([]byte(value))
	return prefix + "-" + hex.EncodeToString(hash[:16])
}

func writeAPIError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(w, status, contracts.Error{Code: code, Message: message, Retryable: retryable})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "response could not be encoded", http.StatusInternalServerError)
		return
	}
	if len(data) > contracts.MaxEnvelopeBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "response_too_large", "response exceeds the size limit", false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
