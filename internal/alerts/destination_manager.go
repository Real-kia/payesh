package alerts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

var (
	// ErrDestinationManaged reports an attempt to change an environment-managed
	// destination.
	ErrDestinationManaged = errors.New("this destination is managed by the environment and cannot be changed here")
	// ErrTestThrottled reports test sends that are too frequent.
	ErrTestThrottled = errors.New("wait a few seconds before sending another test")
)

const testInterval = 3 * time.Second

// DestinationInput is a create or partial-update request. Omitted fields keep
// their value on update. URL, Secret and Headers are write-only.
type DestinationInput struct {
	Name                *string             `json:"name,omitempty"`
	Kind                *string             `json:"kind,omitempty"`
	URL                 *string             `json:"url,omitempty"`
	Secret              *string             `json:"secret,omitempty"`
	ChatID              *string             `json:"chat_id,omitempty"`
	Headers             *map[string]*string `json:"headers,omitempty"`
	Format              *string             `json:"format,omitempty"`
	Template            *string             `json:"template,omitempty"`
	AllowInsecureHTTP   *bool               `json:"allow_insecure_http,omitempty"`
	AllowPrivateNetwork *bool               `json:"allow_private_network,omitempty"`
	TimeoutSeconds      *int                `json:"timeout_seconds,omitempty"`
	MaxAttempts         *int                `json:"max_attempts,omitempty"`
	RetryBackoffSeconds *int                `json:"retry_backoff_seconds,omitempty"`
	MaxPerMinute        *int                `json:"max_per_minute,omitempty"`
	Filter              *DestinationFilter  `json:"filter,omitempty"`
	Enabled             *bool               `json:"enabled,omitempty"`
}

// DeliveryStatusView is the last delivery outcome shown in the UI and API.
type DeliveryStatusView struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Status string    `json:"status"`
	Error  string    `json:"error,omitempty"`
}

// DestinationView is the API representation. It never contains the URL path,
// secret, or header values.
type DestinationView struct {
	ID                  string              `json:"id"`
	Name                string              `json:"name"`
	Kind                string              `json:"kind"`
	URLPreview          string              `json:"url_preview"`
	HasSecret           bool                `json:"has_secret"`
	ChatID              string              `json:"chat_id,omitempty"`
	HeaderNames         []string            `json:"header_names"`
	Format              string              `json:"format"`
	Template            string              `json:"template,omitempty"`
	AllowInsecureHTTP   bool                `json:"allow_insecure_http"`
	AllowPrivateNetwork bool                `json:"allow_private_network"`
	TimeoutSeconds      int                 `json:"timeout_seconds"`
	MaxAttempts         int                 `json:"max_attempts"`
	RetryBackoffSeconds int                 `json:"retry_backoff_seconds"`
	MaxPerMinute        int                 `json:"max_per_minute"`
	Filter              DestinationFilter   `json:"filter"`
	Enabled             bool                `json:"enabled"`
	Managed             bool                `json:"managed"`
	SecretsUnreadable   bool                `json:"secrets_unreadable,omitempty"`
	CreatedAt           *time.Time          `json:"created_at,omitempty"`
	UpdatedAt           *time.Time          `json:"updated_at,omitempty"`
	LastDelivery        *DeliveryStatusView `json:"last_delivery,omitempty"`
}

// TestResult is the outcome of a "send test" request.
type TestResult struct {
	OK         bool   `json:"ok"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// destinationConfig is the non-secret JSON stored beside the encrypted blob.
type destinationConfig struct {
	Kind                string            `json:"kind"`
	URLPreview          string            `json:"url_preview"`
	HasSecret           bool              `json:"has_secret"`
	ChatID              string            `json:"chat_id,omitempty"`
	HeaderNames         []string          `json:"header_names,omitempty"`
	Format              string            `json:"format"`
	Template            string            `json:"template,omitempty"`
	AllowInsecureHTTP   bool              `json:"allow_insecure_http,omitempty"`
	AllowPrivateNetwork bool              `json:"allow_private_network,omitempty"`
	TimeoutSeconds      int               `json:"timeout_seconds"`
	MaxAttempts         int               `json:"max_attempts"`
	RetryBackoffSeconds int               `json:"retry_backoff_seconds"`
	MaxPerMinute        int               `json:"max_per_minute"`
	Filter              DestinationFilter `json:"filter"`
}

// DestinationManager owns owner-configured notification destinations: the
// encrypted store, the environment-managed entries, delivery status, and the
// provider the alert engine reads.
type DestinationManager struct {
	Store *monitoring.Store
	Vault *Vault
	// VaultPath lets a manager without a key yet pick it up later, once the
	// server has created it (command line tools never create the key).
	VaultPath string
	Notifier  Notifier
	// Env holds destinations configured through environment variables. They
	// are shown read-only.
	Env []NotificationDestination

	mu        sync.Mutex
	envStatus map[string]DeliveryStatusView
	lastTest  map[string]time.Time
	now       func() time.Time
}

// NewDestinationManager prepares a manager. env entries get stable IDs.
func NewDestinationManager(store *monitoring.Store, vault *Vault, env []NotificationDestination) *DestinationManager {
	managed := make([]NotificationDestination, 0, len(env))
	for index, destination := range env {
		destination.Managed = true
		if destination.ID == "" {
			destination.ID = "env-" + destination.Kind
			if index > 0 && destination.ID == managed[index-1].ID {
				destination.ID += "-2"
			}
		}
		if destination.Name == "" {
			destination.Name = strings.ToUpper(destination.Kind[:1]) + destination.Kind[1:] + " (environment)"
		}
		// Operator-supplied process configuration is trusted: it keeps working
		// for private receivers exactly as before this feature.
		destination.AllowPrivateNetwork = true
		managed = append(managed, destination)
	}
	return &DestinationManager{Store: store, Vault: vault, Env: managed, envStatus: map[string]DeliveryStatusView{}, lastTest: map[string]time.Time{}, now: func() time.Time { return time.Now().UTC() }}
}

var errNoVault = errors.New("the notification secret key is not available yet")

func (m *DestinationManager) vault() (*Vault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Vault != nil {
		return m.Vault, nil
	}
	if m.VaultPath == "" {
		return nil, errNoVault
	}
	vault, err := OpenExistingVault(m.VaultPath)
	if err != nil {
		return nil, err
	}
	if vault == nil {
		return nil, errNoVault
	}
	m.Vault = vault
	return vault, nil
}

func newDestinationID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "dst_" + hex.EncodeToString(buf)
}

func urlPreview(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	preview := parsed.Scheme + "://" + parsed.Host
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" {
		preview += "/…"
	}
	return preview
}

func headerNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *DestinationManager) envView(destination NotificationDestination) DestinationView {
	destination = destination.withDefaults()
	view := DestinationView{ID: destination.ID, Name: destination.Name, Kind: destination.Kind, URLPreview: urlPreview(destination.URL), HasSecret: destination.Secret != "",
		ChatID: destination.ChatID, HeaderNames: []string{}, Format: destination.Format, AllowPrivateNetwork: true, TimeoutSeconds: int(destination.Timeout / time.Second),
		MaxAttempts: destination.MaxAttempts, RetryBackoffSeconds: int(destination.RetryBackoff / time.Second), MaxPerMinute: destination.MaxPerMinute, Filter: destination.Filter, Enabled: destination.Enabled, Managed: true}
	m.mu.Lock()
	if status, found := m.envStatus[destination.ID]; found {
		copy := status
		view.LastDelivery = &copy
	}
	m.mu.Unlock()
	return view
}

func rowView(row monitoring.AlertDestinationRow, config destinationConfig) DestinationView {
	view := DestinationView{ID: row.ID, Name: row.Name, Kind: config.Kind, URLPreview: config.URLPreview, HasSecret: config.HasSecret, ChatID: config.ChatID,
		HeaderNames: config.HeaderNames, Format: config.Format, Template: config.Template, AllowInsecureHTTP: config.AllowInsecureHTTP, AllowPrivateNetwork: config.AllowPrivateNetwork,
		TimeoutSeconds: config.TimeoutSeconds, MaxAttempts: config.MaxAttempts, RetryBackoffSeconds: config.RetryBackoffSeconds, MaxPerMinute: config.MaxPerMinute, Filter: config.Filter, Enabled: row.Enabled}
	if view.HeaderNames == nil {
		view.HeaderNames = []string{}
	}
	created, updated := row.CreatedAt, row.UpdatedAt
	view.CreatedAt, view.UpdatedAt = &created, &updated
	if !row.LastAttemptAt.IsZero() {
		view.LastDelivery = &DeliveryStatusView{At: row.LastAttemptAt, OK: row.LastOK, Status: row.LastStatus, Error: row.LastError}
	}
	return view
}

func (m *DestinationManager) viewOf(row monitoring.AlertDestinationRow) DestinationView {
	var config destinationConfig
	if err := json.Unmarshal([]byte(row.ConfigJSON), &config); err != nil {
		return DestinationView{ID: row.ID, Name: row.Name, Enabled: row.Enabled, HeaderNames: []string{}, SecretsUnreadable: true}
	}
	view := rowView(row, config)
	if vault, err := m.vault(); err != nil {
		view.SecretsUnreadable = true
	} else if _, err := vault.open(row.ID, row.SecretBlob); err != nil {
		view.SecretsUnreadable = true
	}
	return view
}

// List returns environment-managed destinations first, then stored ones.
func (m *DestinationManager) List(ctx context.Context) ([]DestinationView, error) {
	rows, err := m.Store.ListAlertDestinations(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]DestinationView, 0, len(m.Env)+len(rows))
	for _, destination := range m.Env {
		views = append(views, m.envView(destination))
	}
	for _, row := range rows {
		views = append(views, m.viewOf(row))
	}
	return views, nil
}

// Get returns one destination.
func (m *DestinationManager) Get(ctx context.Context, id string) (DestinationView, error) {
	for _, destination := range m.Env {
		if destination.ID == id {
			return m.envView(destination), nil
		}
	}
	row, err := m.Store.GetAlertDestination(ctx, id)
	if err != nil {
		return DestinationView{}, err
	}
	return m.viewOf(row), nil
}

func (m *DestinationManager) runtime(row monitoring.AlertDestinationRow) (NotificationDestination, error) {
	var config destinationConfig
	if err := json.Unmarshal([]byte(row.ConfigJSON), &config); err != nil {
		return NotificationDestination{}, errors.New("stored destination is unreadable")
	}
	vault, err := m.vault()
	if err != nil {
		return NotificationDestination{}, err
	}
	secrets, err := vault.open(row.ID, row.SecretBlob)
	if err != nil {
		return NotificationDestination{}, err
	}
	return NotificationDestination{ID: row.ID, Name: row.Name, Kind: config.Kind, URL: secrets.URL, Secret: secrets.Secret, ChatID: config.ChatID, Enabled: row.Enabled,
		Format: config.Format, Template: config.Template, Headers: secrets.Headers, AllowInsecureHTTP: config.AllowInsecureHTTP, AllowPrivateNetwork: config.AllowPrivateNetwork,
		Timeout: time.Duration(config.TimeoutSeconds) * time.Second, MaxAttempts: config.MaxAttempts, RetryBackoff: time.Duration(config.RetryBackoffSeconds) * time.Second,
		MaxPerMinute: config.MaxPerMinute, Filter: config.Filter}, nil
}

// apply merges a request into a runtime destination and validates the result.
func apply(current NotificationDestination, in DestinationInput, creating bool) (NotificationDestination, error) {
	if in.Kind != nil {
		if !creating && *in.Kind != current.Kind {
			return current, errors.New("the kind of a destination cannot be changed")
		}
		current.Kind = *in.Kind
	}
	if in.Name != nil {
		current.Name = strings.TrimSpace(*in.Name)
	}
	if in.URL != nil {
		current.URL = strings.TrimSpace(*in.URL)
	}
	if in.Secret != nil {
		current.Secret = *in.Secret
	}
	if in.ChatID != nil {
		current.ChatID = strings.TrimSpace(*in.ChatID)
	}
	if in.Headers != nil {
		next := map[string]string{}
		for name, value := range *in.Headers {
			if value != nil {
				next[name] = *value
			} else if existing, found := current.Headers[name]; found {
				next[name] = existing
			} else {
				return current, errors.New("header " + name + " has no value")
			}
		}
		current.Headers = next
	}
	if in.Format != nil {
		current.Format = *in.Format
	}
	if in.Template != nil {
		current.Template = *in.Template
	}
	if in.AllowInsecureHTTP != nil {
		current.AllowInsecureHTTP = *in.AllowInsecureHTTP
	}
	if in.AllowPrivateNetwork != nil {
		current.AllowPrivateNetwork = *in.AllowPrivateNetwork
	}
	if in.TimeoutSeconds != nil {
		current.Timeout = time.Duration(*in.TimeoutSeconds) * time.Second
	}
	if in.MaxAttempts != nil {
		current.MaxAttempts = *in.MaxAttempts
	}
	if in.RetryBackoffSeconds != nil {
		current.RetryBackoff = time.Duration(*in.RetryBackoffSeconds) * time.Second
	}
	if in.MaxPerMinute != nil {
		current.MaxPerMinute = *in.MaxPerMinute
	}
	if in.Filter != nil {
		current.Filter = *in.Filter
	}
	if in.Enabled != nil {
		current.Enabled = *in.Enabled
	}
	// A template is only meaningful for the template format; drop a stale one
	// when the format changes away from it.
	if current.Format != FormatTemplate {
		current.Template = ""
	}
	current = current.withDefaults()
	if err := current.Validate(); err != nil {
		return current, err
	}
	if err := current.Filter.Validate(); err != nil {
		return current, err
	}
	return current, nil
}

func (m *DestinationManager) persist(destination NotificationDestination, row monitoring.AlertDestinationRow) (monitoring.AlertDestinationRow, error) {
	config := destinationConfig{Kind: destination.Kind, URLPreview: urlPreview(destination.URL), HasSecret: destination.Secret != "", ChatID: destination.ChatID,
		HeaderNames: headerNames(destination.Headers), Format: destination.Format, Template: destination.Template, AllowInsecureHTTP: destination.AllowInsecureHTTP,
		AllowPrivateNetwork: destination.AllowPrivateNetwork, TimeoutSeconds: int(destination.Timeout / time.Second), MaxAttempts: destination.MaxAttempts,
		RetryBackoffSeconds: int(destination.RetryBackoff / time.Second), MaxPerMinute: destination.MaxPerMinute, Filter: destination.Filter}
	encoded, err := json.Marshal(config)
	if err != nil {
		return row, err
	}
	vault, err := m.vault()
	if err != nil {
		return row, err
	}
	blob, err := vault.seal(row.ID, destinationSecrets{URL: destination.URL, Secret: destination.Secret, Headers: destination.Headers})
	if err != nil {
		return row, err
	}
	row.Name, row.ConfigJSON, row.SecretBlob, row.Enabled = destination.Name, string(encoded), blob, destination.Enabled
	return row, nil
}

// Create validates and stores a destination.
func (m *DestinationManager) Create(ctx context.Context, in DestinationInput) (DestinationView, error) {
	if in.Kind == nil || in.URL == nil {
		return DestinationView{}, wrapInvalid(errors.New("kind and url are required"))
	}
	enabled := true
	destination, err := apply(NotificationDestination{Enabled: enabled}, in, true)
	if err != nil {
		return DestinationView{}, wrapInvalid(err)
	}
	if destination.Name == "" {
		destination.Name = destinationDisplayName(destination)
	}
	now := m.now()
	row, err := m.persist(destination, monitoring.AlertDestinationRow{ID: newDestinationID(), CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return DestinationView{}, err
	}
	if err := m.Store.CreateAlertDestination(ctx, row); err != nil {
		return DestinationView{}, err
	}
	return m.viewOf(row), nil
}

func destinationDisplayName(destination NotificationDestination) string {
	if parsed, err := url.Parse(destination.URL); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return destination.Kind
}

// InvalidDestinationError reports a request that failed validation. Its
// message is safe to show to the owner: it never contains secrets.
type InvalidDestinationError struct{ Msg string }

func (e InvalidDestinationError) Error() string { return e.Msg }

func wrapInvalid(err error) error { return InvalidDestinationError{Msg: err.Error()} }

// Update applies a partial update to a stored destination.
func (m *DestinationManager) Update(ctx context.Context, id string, in DestinationInput) (DestinationView, error) {
	if m.isManaged(id) {
		return DestinationView{}, ErrDestinationManaged
	}
	row, err := m.Store.GetAlertDestination(ctx, id)
	if err != nil {
		return DestinationView{}, err
	}
	current, err := m.runtime(row)
	if err != nil {
		// Without readable secrets only a fresh URL (and secret) can repair it.
		if in.URL == nil {
			return DestinationView{}, wrapInvalid(errors.New("stored secrets are unreadable; enter the URL, secret and headers again"))
		}
		var config destinationConfig
		_ = json.Unmarshal([]byte(row.ConfigJSON), &config)
		current = NotificationDestination{ID: row.ID, Name: row.Name, Kind: config.Kind, ChatID: config.ChatID, Enabled: row.Enabled, Format: config.Format, Template: config.Template,
			AllowInsecureHTTP: config.AllowInsecureHTTP, AllowPrivateNetwork: config.AllowPrivateNetwork, Timeout: time.Duration(config.TimeoutSeconds) * time.Second,
			MaxAttempts: config.MaxAttempts, RetryBackoff: time.Duration(config.RetryBackoffSeconds) * time.Second, MaxPerMinute: config.MaxPerMinute, Filter: config.Filter}
	}
	updated, err := apply(current, in, false)
	if err != nil {
		return DestinationView{}, wrapInvalid(err)
	}
	if updated.Name == "" {
		updated.Name = destinationDisplayName(updated)
	}
	row.UpdatedAt = m.now()
	row, err = m.persist(updated, row)
	if err != nil {
		return DestinationView{}, err
	}
	if err := m.Store.UpdateAlertDestination(ctx, row); err != nil {
		return DestinationView{}, err
	}
	return m.viewOf(row), nil
}

// Delete removes a stored destination.
func (m *DestinationManager) Delete(ctx context.Context, id string) error {
	if m.isManaged(id) {
		return ErrDestinationManaged
	}
	return m.Store.DeleteAlertDestination(ctx, id)
}

func (m *DestinationManager) isManaged(id string) bool {
	for _, destination := range m.Env {
		if destination.ID == id {
			return true
		}
	}
	return false
}

func (m *DestinationManager) find(ctx context.Context, id string) (NotificationDestination, error) {
	for _, destination := range m.Env {
		if destination.ID == id {
			return destination, nil
		}
	}
	row, err := m.Store.GetAlertDestination(ctx, id)
	if err != nil {
		return NotificationDestination{}, err
	}
	return m.runtime(row)
}

// Test sends one test notification synchronously (a single attempt) and
// records the outcome as the destination's last delivery.
func (m *DestinationManager) Test(ctx context.Context, id string) (TestResult, error) {
	destination, err := m.find(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	m.mu.Lock()
	if last, found := m.lastTest[id]; found && m.now().Sub(last) < testInterval {
		m.mu.Unlock()
		return TestResult{}, ErrTestThrottled
	}
	m.lastTest[id] = m.now()
	m.mu.Unlock()

	destination.Enabled = true
	destination.MaxAttempts = 1
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	notification := Notification{Kind: NotifyTest, RuleName: "Test notification", ServerName: "payesh",
		Event: contracts.AlertHistoryEvent{ID: "test-" + hex.EncodeToString(buf), AlertID: "payesh-test", State: "firing", OccurredAt: m.now(), Reason: "test_notification"}}
	result := m.Notifier.Deliver(ctx, destination, notification)
	m.Observe(destination, result)
	return TestResult{OK: result.OK, Status: result.Status(), Error: result.Error, DurationMS: result.Duration.Milliseconds()}, nil
}

// Observe records a delivery outcome for the destination's status display.
func (m *DestinationManager) Observe(destination NotificationDestination, result DeliveryResult) {
	if destination.ID == "" {
		return
	}
	detail := truncateRunes(result.Error, 200)
	now := m.now()
	if destination.Managed || m.isManaged(destination.ID) {
		m.mu.Lock()
		m.envStatus[destination.ID] = DeliveryStatusView{At: now, OK: result.OK, Status: result.Status(), Error: detail}
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.Store.RecordAlertDestinationDelivery(ctx, destination.ID, now, result.OK, result.Status(), detail)
}

// Provider returns the enabled destinations for the alert engine. Routing
// filters are applied by the engine, which knows the rule and server.
func (m *DestinationManager) Provider() func(context.Context, contracts.ServerID) []NotificationDestination {
	return func(ctx context.Context, _ contracts.ServerID) []NotificationDestination {
		destinations := make([]NotificationDestination, 0, len(m.Env)+4)
		for _, destination := range m.Env {
			if destination.Enabled {
				destinations = append(destinations, destination.withDefaults())
			}
		}
		rows, err := m.Store.ListAlertDestinations(ctx)
		if err != nil {
			return destinations
		}
		for _, row := range rows {
			if !row.Enabled {
				continue
			}
			destination, err := m.runtime(row)
			if err != nil {
				continue
			}
			destinations = append(destinations, destination.withDefaults())
		}
		return destinations
	}
}

// PreviewPayload renders the body a format would send for a sample alert, so
// the owner can check a template before saving it.
func PreviewPayload(kind, format, templateText string) (string, string, error) {
	destination := NotificationDestination{Kind: kind, Format: format, Template: templateText, ChatID: "preview"}
	if destination.Kind == "" {
		destination.Kind = KindWebhook
	}
	if destination.Format == FormatTemplate {
		if _, err := compileTemplate(templateText); err != nil {
			return "", "", wrapInvalid(err)
		}
	}
	threshold, value := 90.0, 95.2
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	sample := Notification{Kind: NotifyFiring, RuleID: "rule-cpu", RuleName: "High CPU", Expression: "cpu.percent > 90", Metric: "cpu.percent", Operator: ">", Threshold: &threshold,
		ServerName: "web-1", Tags: []string{"web", "prod"}, Value: &value, Since: &since, IncidentURL: "https://payesh.example.com/alerts?incident=inc-1",
		Event: contracts.AlertHistoryEvent{ID: "evt-1", AlertID: "alert-1", ServerID: "srv-1", State: "firing", OccurredAt: since.Add(time.Minute), Reason: "sustained_condition", IncidentID: "inc-1"}}
	body, err := render(destination, sample)
	if err != nil {
		return "", "", wrapInvalid(err)
	}
	return string(body.body), body.contentType, nil
}
