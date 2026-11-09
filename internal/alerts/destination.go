package alerts

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Real-kia/payesh/internal/contracts"
)

// Destination kinds and payload formats.
const (
	KindWebhook  = "webhook"
	KindTelegram = "telegram"

	FormatPayesh   = "payesh"
	FormatTemplate = "template"
	FormatSlack    = "slack"
	FormatDiscord  = "discord"
	FormatNtfy     = "ntfy"
	FormatText     = "text"

	NotifyFiring    = "firing"
	NotifyRecovered = "recovered"
	NotifyReminder  = "reminder"
	NotifyTest      = "test"
	NotifyDigest    = "digest"

	DefaultDeliveryTimeout = 10 * time.Second
	MaxDeliveryTimeout     = 30 * time.Second
	DefaultMaxAttempts     = 3
	MaxAttemptsLimit       = 5
	DefaultRetryBackoff    = time.Second
	MaxRetryBackoff        = 30 * time.Second
	DefaultMaxPerMinute    = 30
	MaxPerMinuteLimit      = 120
	maxFilterEntries       = 32
	maxTemplateBytes       = 4096
	maxDestinationName     = 80
)

var validFormats = map[string]bool{FormatPayesh: true, FormatTemplate: true, FormatSlack: true, FormatDiscord: true, FormatNtfy: true, FormatText: true}

// DestinationFilter limits which notifications reach a destination. Empty
// lists match everything; non-empty lists are "any of" and combine with AND.
type DestinationFilter struct {
	// Kinds are notification kinds: firing, recovered, reminder.
	Kinds []string `json:"kinds,omitempty"`
	// Tags match servers carrying any of these tags (case-insensitive).
	Tags []string `json:"tags,omitempty"`
	// Rules match an alert rule by ID or (case-insensitive) name.
	Rules []string `json:"rules,omitempty"`
	// Servers match server IDs.
	Servers []contracts.ServerID `json:"servers,omitempty"`
}

// Matches reports whether a notification passes the filter.
func (f DestinationFilter) Matches(n Notification) bool {
	if len(f.Kinds) > 0 && !containsFold(f.Kinds, n.Kind) {
		return false
	}
	if len(f.Servers) > 0 {
		found := false
		for _, id := range f.Servers {
			if id == n.Event.ServerID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(f.Rules) > 0 && !containsFold(f.Rules, n.RuleID) && !containsFold(f.Rules, n.RuleName) {
		return false
	}
	if len(f.Tags) > 0 {
		found := false
		for _, tag := range n.Tags {
			if containsFold(f.Tags, tag) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}

// Validate normalizes and checks a filter.
func (f *DestinationFilter) Validate() error {
	clean := func(list []string, max int, allowed map[string]bool) ([]string, error) {
		if len(list) > maxFilterEntries {
			return nil, errors.New("too many filter entries")
		}
		out := make([]string, 0, len(list))
		seen := map[string]bool{}
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if utf8.RuneCountInString(item) > max || strings.ContainsFunc(item, func(r rune) bool { return !unicode.IsPrint(r) }) {
				return nil, errors.New("filter entry is empty, too long, or not printable")
			}
			if allowed != nil && !allowed[strings.ToLower(item)] {
				return nil, errors.New("unknown notification kind in filter: use firing, recovered, or reminder")
			}
			key := strings.ToLower(item)
			if !seen[key] {
				seen[key] = true
				out = append(out, item)
			}
		}
		return out, nil
	}
	var err error
	if f.Kinds, err = clean(f.Kinds, 16, map[string]bool{NotifyFiring: true, NotifyRecovered: true, NotifyReminder: true}); err != nil {
		return err
	}
	if f.Tags, err = clean(f.Tags, 32, nil); err != nil {
		return err
	}
	if f.Rules, err = clean(f.Rules, 128, nil); err != nil {
		return err
	}
	servers := make([]string, len(f.Servers))
	for i, id := range f.Servers {
		servers[i] = string(id)
	}
	if servers, err = clean(servers, 128, nil); err != nil {
		return err
	}
	f.Servers = f.Servers[:0]
	for _, id := range servers {
		f.Servers = append(f.Servers, contracts.ServerID(id))
	}
	return nil
}

// NotificationDestination is one delivery target in runtime form. It may
// carry decrypted secrets, so it is never serialized or logged; the API uses
// DestinationView instead.
type NotificationDestination struct {
	ID      string
	Name    string
	Kind    string
	URL     string
	Secret  string
	ChatID  string
	Enabled bool

	Format   string
	Template string
	// Headers are static request headers such as Authorization.
	Headers map[string]string

	AllowInsecureHTTP   bool
	AllowPrivateNetwork bool

	Timeout      time.Duration
	MaxAttempts  int
	RetryBackoff time.Duration
	MaxPerMinute int

	Filter DestinationFilter
	// Managed marks a destination configured through the environment.
	Managed bool
}

// withDefaults fills unset delivery options.
func (d NotificationDestination) withDefaults() NotificationDestination {
	if d.Format == "" {
		d.Format = FormatPayesh
	}
	if d.Timeout <= 0 {
		d.Timeout = DefaultDeliveryTimeout
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	if d.RetryBackoff <= 0 {
		d.RetryBackoff = DefaultRetryBackoff
	}
	if d.MaxPerMinute <= 0 {
		d.MaxPerMinute = DefaultMaxPerMinute
	}
	return d
}

// limiterKey identifies a destination for rate limiting. Destinations without
// an ID (legacy callers) are keyed by their target.
func (d NotificationDestination) limiterKey() string {
	if d.ID != "" {
		return d.ID
	}
	return d.Kind + "\x00" + d.URL + "\x00" + d.ChatID
}

// Validate checks every option and returns the first problem. It does not
// touch the network.
func (d NotificationDestination) Validate() error {
	d = d.withDefaults()
	if d.Kind != KindWebhook && d.Kind != KindTelegram {
		return errors.New("kind must be webhook or telegram")
	}
	if len(d.Name) > maxDestinationName || strings.ContainsFunc(d.Name, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return errors.New("name is too long or not printable")
	}
	if len(d.Secret) > 256 {
		return errors.New("secret is too long")
	}
	if err := validateDestinationURL(d.URL, d.AllowInsecureHTTP, d.AllowPrivateNetwork); err != nil {
		return err
	}
	if d.Kind == KindTelegram {
		if d.Secret == "" || d.ChatID == "" || len(d.ChatID) > 128 {
			return errors.New("telegram destinations need a bot token and chat id")
		}
		if len(d.Headers) > 0 {
			return errors.New("telegram destinations do not accept custom headers")
		}
	} else if d.ChatID != "" {
		return errors.New("chat id is only valid for telegram destinations")
	}
	if !validFormats[d.Format] {
		return errors.New("format must be payesh, template, slack, discord, ntfy, or text")
	}
	if d.Format == FormatTemplate {
		if d.Template == "" {
			return errors.New("a template is required for the template format")
		}
		if _, err := compileTemplate(d.Template); err != nil {
			return err
		}
	} else if d.Template != "" {
		return errors.New("template is only valid for the template format")
	}
	if err := ValidateHeaders(d.Headers); err != nil {
		return err
	}
	if d.Timeout > MaxDeliveryTimeout {
		return errors.New("timeout must be at most 30 seconds")
	}
	if d.MaxAttempts > MaxAttemptsLimit {
		return errors.New("retry attempts must be between 1 and 5")
	}
	if d.RetryBackoff > MaxRetryBackoff {
		return errors.New("retry backoff must be at most 30 seconds")
	}
	if d.MaxPerMinute > MaxPerMinuteLimit {
		return errors.New("rate limit must be at most 120 notifications per minute")
	}
	filter := d.Filter
	return filter.Validate()
}

// Notification is one alert notification with the context the payload and
// routing need.
type Notification struct {
	Event contracts.AlertHistoryEvent
	// Kind is firing, recovered, reminder, test, or digest.
	Kind        string
	RuleID      string
	RuleName    string
	Expression  string
	Metric      string
	Operator    string
	Threshold   *float64
	ServerName  string
	Tags        []string
	Value       *float64
	Since       *time.Time
	IncidentURL string
	// Items holds the folded notifications of a digest.
	Items []Notification
	// Dropped counts notifications a digest could not list.
	Dropped int
}
