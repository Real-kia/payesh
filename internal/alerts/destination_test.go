package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// receiver is a fake notification endpoint that records requests.
type receiver struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	status   func(call int) int
	calls    atomic.Int32
}

type recordedRequest struct {
	Body    string
	Header  http.Header
	Path    string
	Elapsed time.Duration
}

func newReceiver(t *testing.T, status func(call int) int) *receiver {
	t.Helper()
	r := &receiver{status: status}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, recordedRequest{Body: string(body), Header: req.Header.Clone(), Path: req.URL.RequestURI()})
		r.mu.Unlock()
		call := int(r.calls.Add(1))
		code := http.StatusOK
		if r.status != nil {
			code = r.status(call)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) got() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

// localDestination targets a loopback receiver, which needs both explicit
// opt-ins (plain HTTP on a private network).
func localDestination(r *receiver) NotificationDestination {
	return NotificationDestination{ID: "dst_test", Name: "test", Kind: KindWebhook, URL: r.server.URL + "/hook?token=abc", Enabled: true, AllowInsecureHTTP: true, AllowPrivateNetwork: true,
		Timeout: 2 * time.Second, MaxAttempts: 3, RetryBackoff: time.Millisecond}
}

func sampleNotification() Notification {
	threshold, value := 90.0, 95.5
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return Notification{Kind: NotifyFiring, RuleID: "rule-cpu", RuleName: "High CPU", Expression: "cpu.utilization > 90", Metric: "cpu.utilization", Operator: ">", Threshold: &threshold,
		ServerName: "web-1", Tags: []string{"flow", "prod"}, Value: &value, Since: &since, IncidentURL: "https://payesh.example.com/alerts?incident=inc-1",
		Event: contracts.AlertHistoryEvent{ID: "evt-1", AlertID: "alert-1", ServerID: "srv-1", State: "firing", OccurredAt: since.Add(time.Minute), Reason: "sustained_condition", IncidentID: "inc-1"}}
}

func TestWebhookPayloadSignatureAndHeaders(t *testing.T) {
	fake := newReceiver(t, nil)
	destination := localDestination(fake)
	destination.Secret = "shared-secret"
	destination.Headers = map[string]string{"Authorization": "Bearer placeholder-token", "X-Team": "ops"}
	result := (Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	if !result.OK || result.Attempts != 1 || result.StatusCode != 200 {
		t.Fatalf("result=%+v", result)
	}
	request := fake.got()[0]
	if request.Path != "/hook?token=abc" {
		t.Fatalf("path=%q", request.Path)
	}
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	mac.Write([]byte(request.Body))
	if request.Header.Get("X-Payesh-Signature") != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature mismatch: %q", request.Header.Get("X-Payesh-Signature"))
	}
	if request.Header.Get("Authorization") != "Bearer placeholder-token" || request.Header.Get("X-Team") != "ops" {
		t.Fatalf("custom headers missing: %v", request.Header)
	}
	if request.Header.Get("X-Payesh-Event") != "firing" || request.Header.Get("X-Payesh-Delivery") != "evt-1" || request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers=%v", request.Header)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
		t.Fatal(err)
	}
	// Existing consumers keep their four original fields.
	for _, key := range []string{"alert_id", "state", "occurred_at", "reason"} {
		if _, found := payload[key]; !found {
			t.Fatalf("v1 payload lost %q: %s", key, request.Body)
		}
	}
	if payload["schema"] != PayloadSchema || payload["kind"] != "firing" {
		t.Fatalf("payload=%v", payload)
	}
	rule, _ := payload["rule"].(map[string]any)
	server, _ := payload["server"].(map[string]any)
	incident, _ := payload["incident"].(map[string]any)
	if rule["name"] != "High CPU" || rule["threshold"] != 90.0 || server["name"] != "web-1" || incident["url"] != "https://payesh.example.com/alerts?incident=inc-1" || payload["value"] != 95.5 {
		t.Fatalf("context fields wrong: %s", request.Body)
	}
}

func TestRetryPolicy(t *testing.T) {
	cases := []struct {
		name     string
		statuses []int
		attempts int
		ok       bool
	}{
		{"recovers after server errors", []int{500, 502, 200}, 3, true},
		{"retries rate limiting", []int{429, 200}, 2, true},
		{"client errors are final", []int{400, 200}, 1, false},
		{"gives up after max attempts", []int{500, 500, 500, 200}, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			statuses := c.statuses
			fake := newReceiver(t, func(call int) int { return statuses[min(call-1, len(statuses)-1)] })
			result := (Notifier{}).Deliver(context.Background(), localDestination(fake), sampleNotification())
			if result.OK != c.ok || result.Attempts != c.attempts || int(fake.calls.Load()) != c.attempts {
				t.Fatalf("result=%+v calls=%d", result, fake.calls.Load())
			}
			if !c.ok && !strings.Contains(result.Error, "HTTP") {
				t.Fatalf("error=%q", result.Error)
			}
		})
	}
}

func TestSingleAttemptPolicyAndTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	destination := localDestination(&receiver{server: slow})
	destination.Timeout, destination.MaxAttempts = 50*time.Millisecond, 1
	result := (Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	if result.OK || result.Attempts != 1 || result.Error != "timeout" {
		t.Fatalf("result=%+v", result)
	}
}

func TestDeliveryErrorsNeverLeakURLOrSecrets(t *testing.T) {
	destination := NotificationDestination{Kind: KindWebhook, URL: "https://hooks.invalid.example/services/SECRET-PATH-TOKEN", Secret: "shared-secret", Enabled: true,
		Headers: map[string]string{"Authorization": "Bearer HEADER-SECRET"}, MaxAttempts: 1, Timeout: time.Second}
	result := (Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	if result.OK {
		t.Fatal("unexpected success")
	}
	for _, secret := range []string{"SECRET-PATH-TOKEN", "shared-secret", "HEADER-SECRET", "hooks.invalid.example"} {
		if strings.Contains(result.Error, secret) || strings.Contains(result.Err().Error(), secret) {
			t.Fatalf("error leaked %q: %q", secret, result.Error)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	target := newReceiver(t, nil)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.server.URL+"/elsewhere", http.StatusFound)
	}))
	defer redirector.Close()
	destination := localDestination(&receiver{server: redirector})
	destination.MaxAttempts = 1
	result := (Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	if result.OK || result.StatusCode != http.StatusFound || target.calls.Load() != 0 {
		t.Fatalf("redirect followed: result=%+v target calls=%d", result, target.calls.Load())
	}
}

func TestSSRFAddressPolicy(t *testing.T) {
	cases := []struct {
		addr         string
		allowPrivate bool
		plainHTTP    bool
		blocked      bool
	}{
		{"93.184.216.34", false, false, false},
		{"127.0.0.1", false, false, true},
		{"::1", false, false, true},
		{"10.0.0.5", false, false, true},
		{"192.168.1.10", false, false, true},
		{"172.16.0.1", false, false, true},
		{"100.64.0.1", false, false, true},
		{"169.254.169.254", false, false, true},
		{"169.254.169.254", true, false, true}, // metadata is never allowed
		{"fd00:ec2::254", true, false, true},
		{"100.100.100.200", true, false, true},
		{"fe80::1", false, false, true},
		{"0.0.0.0", false, false, true},
		{"::ffff:127.0.0.1", false, false, true},
		{"127.0.0.1", true, false, false},
		{"10.0.0.5", true, true, false},
		{"93.184.216.34", true, true, true}, // plain HTTP never reaches a public address
	}
	for _, c := range cases {
		err := checkDialAddress(netip.MustParseAddr(c.addr), c.allowPrivate, c.plainHTTP)
		if (err != nil) != c.blocked {
			t.Errorf("%s allowPrivate=%v plainHTTP=%v: err=%v want blocked=%v", c.addr, c.allowPrivate, c.plainHTTP, err, c.blocked)
		}
	}
}

func TestDestinationURLValidation(t *testing.T) {
	cases := []struct {
		url          string
		allowHTTP    bool
		allowPrivate bool
		ok           bool
	}{
		{"https://hooks.example.com/x", false, false, true},
		{"http://hooks.example.com/x", false, false, false},
		{"http://10.0.0.5:8080/x", true, false, false},
		{"http://10.0.0.5:8080/x", true, true, true},
		{"https://127.0.0.1/x", false, false, false},
		{"https://127.0.0.1/x", false, true, true},
		{"https://localhost/x", false, false, false},
		{"https://169.254.169.254/latest", false, true, false},
		{"https://[::1]/x", false, false, false},
		{"https://user:pass@example.com/x", false, false, false},
		{"https://example.com/x#frag", false, false, false},
		{"ftp://example.com/x", false, false, false},
		{"https:///nohost", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		err := validateDestinationURL(c.url, c.allowHTTP, c.allowPrivate)
		if (err == nil) != c.ok {
			t.Errorf("%q http=%v private=%v: err=%v want ok=%v", c.url, c.allowHTTP, c.allowPrivate, err, c.ok)
		}
	}
}

func TestGuardedClientBlocksLoopbackAtConnectTime(t *testing.T) {
	fake := newReceiver(t, nil)
	// A hostname that is not blocked statically but resolves to loopback.
	for _, target := range []string{fake.server.URL, strings.Replace(fake.server.URL, "127.0.0.1", "localhost", 1)} {
		client := guardedClient(false, false, time.Second)
		_, err := client.Get(target)
		if err == nil || !errors.Is(err, errBlockedAddress) {
			t.Fatalf("%s: err=%v, want blocked", target, err)
		}
	}
	if fake.calls.Load() != 0 {
		t.Fatal("blocked request reached the receiver")
	}
	// The default notifier path reports it as a permanent, non-retried error.
	destination := NotificationDestination{Kind: KindWebhook, URL: strings.Replace(fake.server.URL, "http://", "https://", 1), Enabled: true, MaxAttempts: 3, RetryBackoff: time.Millisecond}
	result := (Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	if result.OK || result.Attempts != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestHeaderValidation(t *testing.T) {
	good := map[string]string{"Authorization": "Bearer abc", "X-Api-Key": "k"}
	if err := ValidateHeaders(good); err != nil {
		t.Fatal(err)
	}
	for name, headers := range map[string]map[string]string{
		"host":           {"Host": "evil"},
		"content-length": {"Content-Length": "1"},
		"signature":      {"X-Payesh-Signature": "x"},
		"bad name":       {"Bad Name": "x"},
		"crlf value":     {"X-A": "a\r\nInjected: 1"},
		"empty value":    {"X-A": ""},
		"too long value": {"X-A": strings.Repeat("a", 2000)},
		"transfer enc":   {"Transfer-Encoding": "chunked"},
		"too many":       {"A1": "1", "A2": "1", "A3": "1", "A4": "1", "A5": "1", "A6": "1", "A7": "1", "A8": "1", "A9": "1", "A10": "1", "A11": "1"},
	} {
		if ValidateHeaders(headers) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFilterRouting(t *testing.T) {
	n := sampleNotification()
	cases := []struct {
		name   string
		filter DestinationFilter
		match  bool
	}{
		{"default matches all", DestinationFilter{}, true},
		{"kind match", DestinationFilter{Kinds: []string{"firing"}}, true},
		{"kind mismatch", DestinationFilter{Kinds: []string{"recovered", "reminder"}}, false},
		{"tag match is case-insensitive", DestinationFilter{Tags: []string{"FLOW"}}, true},
		{"tag mismatch", DestinationFilter{Tags: []string{"staging"}}, false},
		{"rule by name", DestinationFilter{Rules: []string{"high cpu"}}, true},
		{"rule by id", DestinationFilter{Rules: []string{"rule-cpu"}}, true},
		{"rule mismatch", DestinationFilter{Rules: []string{"disk"}}, false},
		{"server match", DestinationFilter{Servers: []contracts.ServerID{"srv-1"}}, true},
		{"server mismatch", DestinationFilter{Servers: []contracts.ServerID{"srv-2"}}, false},
		{"all must hold", DestinationFilter{Kinds: []string{"firing"}, Tags: []string{"flow"}, Rules: []string{"disk"}}, false},
	}
	for _, c := range cases {
		if got := c.filter.Matches(n); got != c.match {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	bad := DestinationFilter{Kinds: []string{"panic"}}
	if bad.Validate() == nil {
		t.Error("unknown kind accepted")
	}
	noTags := n
	noTags.Tags = nil
	if (DestinationFilter{Tags: []string{"flow"}}).Matches(noTags) {
		t.Error("untagged server matched a tag filter")
	}
}

func TestTemplateFormat(t *testing.T) {
	fake := newReceiver(t, nil)
	destination := localDestination(fake)
	destination.Format = FormatTemplate
	destination.Template = `{"text":{{json .Title}},"rule":{{json .Rule}},"server":{{json .Server}},"tags":{{json .Tags}},"state":{{json .State}},"value":{{json .Value}},"threshold":{{json .Threshold}},"since":{{json .Since}},"url":{{json .IncidentURL}}}`
	if err := destination.Validate(); err != nil {
		t.Fatal(err)
	}
	result := (Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	if !result.OK {
		t.Fatalf("result=%+v", result)
	}
	request := fake.got()[0]
	var payload map[string]any
	if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
		t.Fatalf("template output is not JSON: %v: %s", err, request.Body)
	}
	if payload["rule"] != "High CPU" || payload["server"] != "web-1" || payload["state"] != "firing" || payload["value"] != "95.5" || payload["threshold"] != "90" ||
		payload["since"] != "2026-01-02T03:04:05Z" || payload["url"] != "https://payesh.example.com/alerts?incident=inc-1" {
		t.Fatalf("payload=%s", request.Body)
	}
	if tags, _ := payload["tags"].([]any); len(tags) != 2 || tags[0] != "flow" {
		t.Fatalf("tags=%v", payload["tags"])
	}
	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type=%q", request.Header.Get("Content-Type"))
	}
	// Plain-text templates are sent as text.
	destination.Template = `{{.Summary}}`
	if err := destination.Validate(); err != nil {
		t.Fatal(err)
	}
	(Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	second := fake.got()[1]
	if second.Header.Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(second.Body, "[FIRING] High CPU on web-1") {
		t.Fatalf("text template: %q %q", second.Header.Get("Content-Type"), second.Body)
	}
}

func TestTemplateSandbox(t *testing.T) {
	bad := map[string]string{
		"empty":                 "",
		"unknown field":         `{{.Password}}`,
		"unknown function":      `{{exec "id"}}`,
		"syntax error":          `{{.Rule`,
		"define":                `{{define "x"}}a{{end}}{{template "x"}}`,
		"template call":         `{{template "alert"}}`,
		"block":                 `{{block "x" .}}a{{end}}`,
		"range over number":     `{{range 1000000000}}x{{end}}`,
		"nested range number":   `{{if .Digest}}{{range 99999999}}{{end}}{{end}}`,
		"call a non function":   `{{call .Rule}}`,
		"oversized":             strings.Repeat("a", maxTemplateBytes+1),
		"output over the limit": `{{range .Items}}{{.Summary}}{{end}}` + strings.Repeat("x", 100) + `{{range .Tags}}{{range .}}{{end}}{{end}}` + `{{printf "%20000s" "x"}}`,
	}
	for name, text := range bad {
		destination := NotificationDestination{Kind: KindWebhook, URL: "https://example.com/x", Format: FormatTemplate, Template: text}
		if destination.Validate() == nil {
			t.Errorf("%s: template accepted", name)
		}
	}
	good := `{{if eq .Kind "recovered"}}OK{{else}}BAD{{end}} {{range .Tags}}#{{.}} {{end}}{{upper .Server}} {{truncate 5 .Summary}} {{join .Tags ","}}`
	destination := NotificationDestination{Kind: KindWebhook, URL: "https://example.com/x", Format: FormatTemplate, Template: good}
	if err := destination.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := render(destination, sampleNotification())
	if err != nil || !strings.HasPrefix(string(body.body), "BAD #flow #prod WEB-1") {
		t.Fatalf("body=%q err=%v", body.body, err)
	}
}

func TestPresets(t *testing.T) {
	n := sampleNotification()
	json2 := func(format string) map[string]any {
		body, err := render(NotificationDestination{Kind: KindWebhook, Format: format}, n)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(body.body, &out); err != nil {
			t.Fatalf("%s: %v: %s", format, err, body.body)
		}
		return out
	}
	for _, format := range []string{FormatSlack, FormatText} {
		out := json2(format)
		if text, _ := out["text"].(string); !strings.Contains(text, "[FIRING] High CPU on web-1 (cpu.utilization > 90), value 95.5") || !strings.Contains(text, "https://payesh.example.com/alerts?incident=inc-1") {
			t.Fatalf("%s text=%v", format, out)
		}
	}
	discord := json2(FormatDiscord)
	if content, _ := discord["content"].(string); !strings.Contains(content, "High CPU") {
		t.Fatalf("discord=%v", discord)
	}
	ntfy, err := render(NotificationDestination{Kind: KindWebhook, Format: FormatNtfy}, n)
	if err != nil {
		t.Fatal(err)
	}
	if ntfy.headers["Priority"] != "4" || ntfy.headers["Tags"] != "rotating_light" || !strings.Contains(ntfy.headers["Title"], "High CPU") || ntfy.contentType != "text/plain; charset=utf-8" || !strings.Contains(string(ntfy.body), "High CPU") {
		t.Fatalf("ntfy=%+v", ntfy)
	}
	recovered := n
	recovered.Kind = NotifyRecovered
	ntfy, _ = render(NotificationDestination{Kind: KindWebhook, Format: FormatNtfy}, recovered)
	if ntfy.headers["Tags"] != "white_check_mark" {
		t.Fatalf("ntfy recovered headers=%v", ntfy.headers)
	}
	long := n
	long.RuleName = strings.Repeat("é", 5000)
	if body, _ := render(NotificationDestination{Kind: KindWebhook, Format: FormatDiscord}, long); len([]rune(json2contentLen(body.body))) > 2000 {
		t.Fatal("discord content exceeds the 2000 character limit")
	}
}

func json2contentLen(body []byte) string {
	var out map[string]string
	_ = json.Unmarshal(body, &out)
	return out["content"]
}

func TestTelegramKeepsTokenOutOfErrors(t *testing.T) {
	destination := NotificationDestination{Kind: KindTelegram, URL: "https://127.0.0.1:1", Secret: "123:BOT-TOKEN", ChatID: "42", Enabled: true, AllowPrivateNetwork: true, MaxAttempts: 1, Timeout: time.Second}
	result := (Notifier{}).Deliver(context.Background(), destination, sampleNotification())
	if result.OK || strings.Contains(result.Error, "BOT-TOKEN") {
		t.Fatalf("result=%+v", result)
	}
	if err := (NotificationDestination{Kind: KindTelegram, URL: "https://api.telegram.org"}).Validate(); err == nil {
		t.Fatal("telegram without token accepted")
	}
}

func TestRateLimitGroupsBurstsIntoDigest(t *testing.T) {
	queue := NewDeliveryQueue(Notifier{}, 2)
	queue.hold = 20 * time.Millisecond
	var mu sync.Mutex
	var singles, digests int
	var digestItems []int
	queue.send = func(_ context.Context, _ NotificationDestination, n Notification) error {
		mu.Lock()
		defer mu.Unlock()
		if n.Kind == NotifyDigest {
			digests++
			digestItems = append(digestItems, len(n.Items)+n.Dropped)
		} else {
			singles++
		}
		return nil
	}
	defer queue.Close()
	destination := NotificationDestination{ID: "dst_rate", Kind: KindWebhook, URL: "https://example.com/x", Enabled: true, MaxPerMinute: 600}
	var completed atomic.Int32
	const total = 40
	for i := 0; i < total; i++ {
		n := sampleNotification()
		n.Event.ID = "evt-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := queue.enqueue(context.Background(), destination, n, func(err error) {
			if err != nil {
				t.Errorf("completion error: %v", err)
			}
			completed.Add(1)
		}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for completed.Load() < total && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if completed.Load() != total {
		t.Fatalf("completed %d of %d (singles=%d digests=%d)", completed.Load(), total, singles, digests)
	}
	if singles > burstTokens+2 || digests < 1 {
		t.Fatalf("burst was not grouped: singles=%d digests=%d", singles, digests)
	}
	folded := 0
	for _, n := range digestItems {
		folded += n
	}
	if singles+folded != total {
		t.Fatalf("notifications lost: singles=%d folded=%d total=%d", singles, folded, total)
	}
}

func TestDigestPayloadFormats(t *testing.T) {
	digest := Notification{Kind: NotifyDigest, Event: contracts.AlertHistoryEvent{ID: "digest-1", AlertID: "a", State: "firing", OccurredAt: time.Now()}, Items: []Notification{sampleNotification(), sampleNotification()}, Dropped: 3}
	body, err := render(NotificationDestination{Kind: KindWebhook, Format: FormatPayesh}, digest)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(body.body, &payload)
	if payload["kind"] != "digest" || payload["count"] != 5.0 || len(payload["items"].([]any)) != 2 {
		t.Fatalf("digest payload=%s", body.body)
	}
	slack, _ := render(NotificationDestination{Kind: KindWebhook, Format: FormatSlack}, digest)
	if !strings.Contains(string(slack.body), "5 alert notifications") || !strings.Contains(string(slack.body), "and 3 more") {
		t.Fatalf("slack digest=%s", slack.body)
	}
}

func TestVault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notification.key")
	vault, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode=%v err=%v", info.Mode(), err)
	}
	secrets := destinationSecrets{URL: "https://hooks.example.com/SECRET", Secret: "s", Headers: map[string]string{"Authorization": "Bearer t"}}
	blob, err := vault.seal("dst_1", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "SECRET") || strings.Contains(string(blob), "Bearer") {
		t.Fatal("blob contains plaintext")
	}
	opened, err := vault.open("dst_1", blob)
	if err != nil || opened.URL != secrets.URL || opened.Headers["Authorization"] != "Bearer t" {
		t.Fatalf("opened=%+v err=%v", opened, err)
	}
	if _, err := vault.open("dst_2", blob); err == nil {
		t.Fatal("blob decrypted under another destination id")
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 1
	if _, err := vault.open("dst_1", tampered); err == nil {
		t.Fatal("tampered blob accepted")
	}
	again, err := OpenVault(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.open("dst_1", blob); err != nil {
		t.Fatalf("key was not reused: %v", err)
	}
	other, _ := OpenVault(filepath.Join(dir, "other.key"))
	if _, err := other.open("dst_1", blob); err == nil {
		t.Fatal("blob decrypted with a different key")
	}
	if err := os.WriteFile(filepath.Join(dir, "short.key"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenVault(filepath.Join(dir, "short.key")); err == nil {
		t.Fatal("invalid key file accepted")
	}
}

func testManager(t *testing.T, env ...NotificationDestination) (*DestinationManager, *monitoring.Store) {
	t.Helper()
	store, err := monitoring.OpenStore(context.Background(), ":memory:", monitoring.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	vault, err := OpenVault(filepath.Join(t.TempDir(), "notification.key"))
	if err != nil {
		t.Fatal(err)
	}
	return NewDestinationManager(store, vault, env), store
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestManagerLifecycleKeepsSecretsWriteOnly(t *testing.T) {
	ctx := context.Background()
	manager, store := testManager(t)
	fake := newReceiver(t, nil)
	headers := map[string]*string{"Authorization": strPtr("Bearer PLACEHOLDER-TOKEN")}
	created, err := manager.Create(ctx, DestinationInput{Name: strPtr("Ops"), Kind: strPtr(KindWebhook), URL: strPtr(fake.server.URL + "/hooks/PATH-SECRET"), Secret: strPtr("HMAC-SECRET"),
		Headers: &headers, AllowInsecureHTTP: boolPtr(true), AllowPrivateNetwork: boolPtr(true), Format: strPtr(FormatSlack),
		Filter: &DestinationFilter{Tags: []string{"flow"}, Kinds: []string{"firing"}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(created)
	for _, secret := range []string{"PATH-SECRET", "HMAC-SECRET", "PLACEHOLDER-TOKEN"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("API view leaked %q: %s", secret, encoded)
		}
	}
	if !created.HasSecret || created.URLPreview != strings.TrimSuffix(fake.server.URL, "/")+"/…" || len(created.HeaderNames) != 1 || created.HeaderNames[0] != "Authorization" || !created.Enabled {
		t.Fatalf("view=%+v", created)
	}
	row, err := store.GetAlertDestination(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PATH-SECRET", "HMAC-SECRET", "PLACEHOLDER-TOKEN"} {
		if strings.Contains(row.ConfigJSON, secret) || strings.Contains(string(row.SecretBlob), secret) {
			t.Fatalf("stored row leaked %q", secret)
		}
	}

	// A partial update keeps every omitted secret, and null keeps a header.
	keep := map[string]*string{"Authorization": nil, "X-Team": strPtr("ops")}
	updated, err := manager.Update(ctx, created.ID, DestinationInput{Name: strPtr("Ops chat"), Headers: &keep, MaxPerMinute: intPtr(10)})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Ops chat" || updated.MaxPerMinute != 10 || len(updated.HeaderNames) != 2 || !updated.HasSecret {
		t.Fatalf("updated=%+v", updated)
	}
	runtime, err := manager.find(ctx, created.ID)
	if err != nil || runtime.Headers["Authorization"] != "Bearer PLACEHOLDER-TOKEN" || runtime.Secret != "HMAC-SECRET" || !strings.HasSuffix(runtime.URL, "/hooks/PATH-SECRET") {
		t.Fatalf("secrets not preserved: %+v err=%v", runtime, err)
	}
	// Send test delivers once, signs, and records the last delivery.
	test, err := manager.Test(ctx, created.ID)
	if err != nil || !test.OK || test.Status != "HTTP 200" {
		t.Fatalf("test=%+v err=%v", test, err)
	}
	got := fake.got()
	if len(got) != 1 || got[0].Header.Get("Authorization") != "Bearer PLACEHOLDER-TOKEN" || got[0].Header.Get("X-Payesh-Event") != "test" || !strings.Contains(got[0].Body, "test notification") {
		t.Fatalf("requests=%+v", got)
	}
	if _, err := manager.Test(ctx, created.ID); !errors.Is(err, ErrTestThrottled) {
		t.Fatalf("immediate second test err=%v", err)
	}
	view, _ := manager.Get(ctx, created.ID)
	if view.LastDelivery == nil || !view.LastDelivery.OK || view.LastDelivery.Status != "HTTP 200" {
		t.Fatalf("last delivery=%+v", view.LastDelivery)
	}
	// Disable removes it from the engine's provider; delete removes the row.
	if _, err := manager.Update(ctx, created.ID, DestinationInput{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if got := manager.Provider()(ctx, "srv"); len(got) != 0 {
		t.Fatalf("disabled destination still provided: %+v", got)
	}
	if _, err := manager.Update(ctx, created.ID, DestinationInput{Enabled: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if got := manager.Provider()(ctx, "srv"); len(got) != 1 || got[0].Filter.Tags[0] != "flow" || got[0].Secret != "HMAC-SECRET" {
		t.Fatalf("provider=%+v", got)
	}
	if err := manager.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Get(ctx, created.ID); !errors.Is(err, monitoring.ErrDestinationNotFound) {
		t.Fatalf("get after delete err=%v", err)
	}
}

func intPtr(i int) *int { return &i }

func TestManagerFailedTestRecordsStatusWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	fake := newReceiver(t, func(int) int { return http.StatusInternalServerError })
	created, err := manager.Create(ctx, DestinationInput{Kind: strPtr(KindWebhook), URL: strPtr(fake.server.URL + "/SECRET-PATH"), AllowInsecureHTTP: boolPtr(true), AllowPrivateNetwork: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	test, err := manager.Test(ctx, created.ID)
	if err != nil || test.OK || test.Status != "HTTP 500" || strings.Contains(test.Error, "SECRET-PATH") {
		t.Fatalf("test=%+v err=%v", test, err)
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("test send retried %d times", fake.calls.Load())
	}
	view, _ := manager.Get(ctx, created.ID)
	if view.LastDelivery == nil || view.LastDelivery.OK || view.LastDelivery.Error == "" {
		t.Fatalf("last delivery=%+v", view.LastDelivery)
	}
}

func TestManagerValidation(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	headerValue := "x"
	bad := map[string]DestinationInput{
		"missing url":           {Kind: strPtr(KindWebhook)},
		"missing kind":          {URL: strPtr("https://example.com/x")},
		"plain http":            {Kind: strPtr(KindWebhook), URL: strPtr("http://example.com/x")},
		"http without private":  {Kind: strPtr(KindWebhook), URL: strPtr("http://10.0.0.1/x"), AllowInsecureHTTP: boolPtr(true)},
		"loopback":              {Kind: strPtr(KindWebhook), URL: strPtr("https://127.0.0.1/x")},
		"metadata":              {Kind: strPtr(KindWebhook), URL: strPtr("https://169.254.169.254/x"), AllowPrivateNetwork: boolPtr(true)},
		"credentials in url":    {Kind: strPtr(KindWebhook), URL: strPtr("https://u:p@example.com/x")},
		"bad format":            {Kind: strPtr(KindWebhook), URL: strPtr("https://example.com/x"), Format: strPtr("xml")},
		"template without text": {Kind: strPtr(KindWebhook), URL: strPtr("https://example.com/x"), Format: strPtr(FormatTemplate)},
		"forbidden header":      {Kind: strPtr(KindWebhook), URL: strPtr("https://example.com/x"), Headers: &map[string]*string{"Host": &headerValue}},
		"timeout too long":      {Kind: strPtr(KindWebhook), URL: strPtr("https://example.com/x"), TimeoutSeconds: intPtr(120)},
		"too many attempts":     {Kind: strPtr(KindWebhook), URL: strPtr("https://example.com/x"), MaxAttempts: intPtr(50)},
		"rate too high":         {Kind: strPtr(KindWebhook), URL: strPtr("https://example.com/x"), MaxPerMinute: intPtr(100000)},
		"bad filter kind":       {Kind: strPtr(KindWebhook), URL: strPtr("https://example.com/x"), Filter: &DestinationFilter{Kinds: []string{"nope"}}},
		"telegram no token":     {Kind: strPtr(KindTelegram), URL: strPtr("https://api.telegram.org"), ChatID: strPtr("1")},
		"unknown kind":          {Kind: strPtr("smtp"), URL: strPtr("https://example.com/x")},
	}
	for name, input := range bad {
		if _, err := manager.Create(ctx, input); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.As(err, new(InvalidDestinationError)) {
			t.Errorf("%s: err %T %v is not a validation error", name, err, err)
		}
	}
	if views, _ := manager.List(ctx); len(views) != 0 {
		t.Fatalf("invalid destinations were stored: %+v", views)
	}
	valid, err := manager.Create(ctx, DestinationInput{Kind: strPtr(KindWebhook), URL: strPtr("https://hooks.example.com/x")})
	if err != nil {
		t.Fatal(err)
	}
	if valid.Name != "hooks.example.com" || valid.Format != FormatPayesh || valid.TimeoutSeconds != 10 || valid.MaxAttempts != 3 || valid.MaxPerMinute != 30 {
		t.Fatalf("defaults=%+v", valid)
	}
	if _, err := manager.Update(ctx, valid.ID, DestinationInput{Kind: strPtr(KindTelegram)}); err == nil {
		t.Fatal("kind change accepted")
	}
	if _, err := manager.Update(ctx, valid.ID, DestinationInput{URL: strPtr("http://example.com/plain")}); err == nil {
		t.Fatal("update to plain http accepted")
	}
	view, _ := manager.Get(ctx, valid.ID)
	if view.URLPreview != "https://hooks.example.com/…" {
		t.Fatalf("failed update changed the destination: %+v", view)
	}
}

func TestManagerLimit(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t)
	for i := 0; i < monitoring.MaxAlertDestinations; i++ {
		if _, err := manager.Create(ctx, DestinationInput{Kind: strPtr(KindWebhook), URL: strPtr("https://hooks.example.com/x")}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := manager.Create(ctx, DestinationInput{Kind: strPtr(KindWebhook), URL: strPtr("https://hooks.example.com/x")}); !errors.Is(err, monitoring.ErrDestinationLimit) {
		t.Fatalf("err=%v", err)
	}
}

func TestEnvironmentDestinationsAreReadOnly(t *testing.T) {
	ctx := context.Background()
	manager, _ := testManager(t,
		NotificationDestination{Kind: KindWebhook, URL: "https://env.example.com/hook/SECRET-PATH", Secret: "env-secret", Enabled: true},
		NotificationDestination{Kind: KindTelegram, URL: "https://api.telegram.org", Secret: "env-bot-token", ChatID: "99", Enabled: true})
	views, err := manager.List(ctx)
	if err != nil || len(views) != 2 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	encoded, _ := json.Marshal(views)
	for _, secret := range []string{"SECRET-PATH", "env-secret", "env-bot-token"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("environment destination leaked %q: %s", secret, encoded)
		}
	}
	if !views[0].Managed || views[0].ID != "env-webhook" || views[1].ID != "env-telegram" || !views[0].Enabled {
		t.Fatalf("views=%+v", views)
	}
	if _, err := manager.Update(ctx, "env-webhook", DestinationInput{Enabled: boolPtr(false)}); !errors.Is(err, ErrDestinationManaged) {
		t.Fatalf("update err=%v", err)
	}
	if err := manager.Delete(ctx, "env-telegram"); !errors.Is(err, ErrDestinationManaged) {
		t.Fatalf("delete err=%v", err)
	}
	if got := manager.Provider()(ctx, "srv"); len(got) != 2 || got[0].Secret != "env-secret" {
		t.Fatalf("provider=%+v", got)
	}
	manager.Observe(manager.Env[0], DeliveryResult{OK: true, StatusCode: 204})
	view, _ := manager.Get(ctx, "env-webhook")
	if view.LastDelivery == nil || view.LastDelivery.Status != "HTTP 204" {
		t.Fatalf("env status=%+v", view.LastDelivery)
	}
}

func TestPreviewPayload(t *testing.T) {
	body, contentType, err := PreviewPayload("webhook", FormatTemplate, `{"t":{{json .Title}}}`)
	if err != nil || contentType != "application/json" || !strings.Contains(body, "High CPU") {
		t.Fatalf("body=%q ct=%q err=%v", body, contentType, err)
	}
	if _, _, err := PreviewPayload("webhook", FormatTemplate, `{{.Nope}}`); err == nil {
		t.Fatal("bad template previewed")
	}
	if body, _, err := PreviewPayload("webhook", FormatNtfy, ""); err != nil || !strings.Contains(body, "High CPU") {
		t.Fatalf("ntfy preview=%q err=%v", body, err)
	}
}

// End to end: a firing alert on a tagged server reaches only the destinations
// whose routing accepts it, with rule and server context in the payload.
func TestEngineRoutesNotificationsByTagKindAndRule(t *testing.T) {
	ctx := context.Background()
	manager, store := testManager(t)
	serverID := alertServer(t, store)
	if _, err := store.SetServerTags(ctx, serverID, []string{"Flow"}); err != nil {
		t.Fatal(err)
	}
	everything := newReceiver(t, nil)
	flowOnly := newReceiver(t, nil)
	stagingOnly := newReceiver(t, nil)
	recoveredOnly := newReceiver(t, nil)
	otherRule := newReceiver(t, nil)
	disabled := newReceiver(t, nil)
	for name, spec := range map[string]struct {
		fake    *receiver
		filter  DestinationFilter
		enabled bool
	}{
		"all":       {everything, DestinationFilter{}, true},
		"flow":      {flowOnly, DestinationFilter{Tags: []string{"flow"}, Kinds: []string{"firing"}}, true},
		"staging":   {stagingOnly, DestinationFilter{Tags: []string{"staging"}}, true},
		"recovered": {recoveredOnly, DestinationFilter{Kinds: []string{"recovered"}}, true},
		"rule":      {otherRule, DestinationFilter{Rules: []string{"some other rule"}}, true},
		"disabled":  {disabled, DestinationFilter{}, false},
	} {
		filter := spec.filter
		if _, err := manager.Create(ctx, DestinationInput{Name: strPtr(name), Kind: strPtr(KindWebhook), URL: strPtr(spec.fake.server.URL + "/hook"), AllowInsecureHTTP: boolPtr(true),
			AllowPrivateNetwork: boolPtr(true), Filter: &filter, Enabled: boolPtr(spec.enabled)}); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := NewEngine(store)
	if err != nil {
		t.Fatal(err)
	}
	engine.PublicURL = "https://payesh.example.com/"
	queue := NewDeliveryQueue(Notifier{}, 2)
	queue.Observer = manager.Observe
	defer queue.Close()
	engine.ConfigureDelivery(queue, manager.Provider())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rule := contracts.AlertRule{ID: "route-rule-0001", Name: "CPU too high", Expression: "cpu.utilization > 90", ServerID: serverID, Enabled: true, DurationSeconds: 1, ReminderSeconds: 0, IdempotencyKey: "route-idem-0001", CreatedAt: base}
	if err := engine.AddRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	observation := func(at time.Time, value float64) Observation {
		return Observation{ServerID: serverID, ObservedAt: at, Values: map[string]float64{"cpu.utilization": value}, Validity: map[string]string{"cpu.utilization": "valid"}, Coverage: 1}
	}
	if _, err := engine.Evaluate(ctx, rule, observation(base, 95), base); err != nil {
		t.Fatal(err)
	}
	if result, err := engine.Evaluate(ctx, rule, observation(base.Add(time.Second), 95), base.Add(time.Second)); err != nil || result.Notification != "firing" {
		t.Fatalf("firing=%+v err=%v", result, err)
	}
	waitFor(t, func() bool { return everything.calls.Load() == 1 && flowOnly.calls.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if stagingOnly.calls.Load() != 0 || recoveredOnly.calls.Load() != 0 || otherRule.calls.Load() != 0 || disabled.calls.Load() != 0 {
		t.Fatalf("filtered destinations were called: staging=%d recovered=%d rule=%d disabled=%d", stagingOnly.calls.Load(), recoveredOnly.calls.Load(), otherRule.calls.Load(), disabled.calls.Load())
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(flowOnly.got()[0].Body), &payload); err != nil {
		t.Fatal(err)
	}
	rulePart, _ := payload["rule"].(map[string]any)
	serverPart, _ := payload["server"].(map[string]any)
	tags, _ := serverPart["tags"].([]any)
	if payload["kind"] != "firing" || rulePart["name"] != "CPU too high" || rulePart["threshold"] != 90.0 || payload["value"] != 95.0 || len(tags) != 1 || tags[0] != "Flow" || serverPart["name"] == "" {
		t.Fatalf("payload=%s", flowOnly.got()[0].Body)
	}
	if incident, _ := payload["incident"].(map[string]any); !strings.HasPrefix(incident["url"].(string), "https://payesh.example.com/alerts?incident=") {
		t.Fatalf("incident=%v", payload["incident"])
	}

	// Recovery reaches the "all" and "recovered" destinations only.
	if _, err := engine.Evaluate(ctx, rule, observation(base.Add(2*time.Second), 10), base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return recoveredOnly.calls.Load() == 1 && everything.calls.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	if flowOnly.calls.Load() != 1 {
		t.Fatalf("firing-only destination got the recovery: %d", flowOnly.calls.Load())
	}
	// Delivery status is recorded for the destinations that were used.
	views, _ := manager.List(ctx)
	for _, view := range views {
		switch view.Name {
		case "all", "flow", "recovered":
			if view.LastDelivery == nil || !view.LastDelivery.OK {
				t.Errorf("%s last delivery=%+v", view.Name, view.LastDelivery)
			}
		case "staging", "rule", "disabled":
			if view.LastDelivery != nil {
				t.Errorf("%s has a delivery it should not: %+v", view.Name, view.LastDelivery)
			}
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
