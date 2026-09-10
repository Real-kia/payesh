package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestNotifierUsesBoundedHTTPSWebhookAndSignature(t *testing.T) {
	secret := "webhook-secret"
	var gotBody string
	var gotSignature string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, MaxNotificationBody))
		gotBody, gotSignature = string(body), r.Header.Get("X-Payesh-Signature")
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	notifier := Notifier{Client: &http.Client{Transport: transport}}
	event := contracts.AlertHistoryEvent{ID: "event-1", AlertID: "rule-1", State: "firing", OccurredAt: time.Now().UTC(), Reason: "sustained_condition"}
	if err := notifier.Send(context.Background(), NotificationDestination{Kind: "webhook", URL: "https://example.test/hook", Secret: secret, Enabled: true}, event); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"alert_id":"rule-1"`) {
		t.Fatalf("body=%s", gotBody)
	}
	hash := hmac.New(sha256.New, []byte(secret))
	_, _ = hash.Write([]byte(gotBody))
	if gotSignature != hex.EncodeToString(hash.Sum(nil)) {
		t.Fatalf("signature=%q", gotSignature)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestNotifierRejectsNonHTTPSAndQueueIsBounded(t *testing.T) {
	notifier := Notifier{}
	event := contracts.AlertHistoryEvent{ID: "event-1", AlertID: "rule-1", State: "firing", OccurredAt: time.Now().UTC()}
	if err := notifier.Send(context.Background(), NotificationDestination{Kind: "webhook", URL: "http://127.0.0.1/hook", Enabled: true}, event); err == nil {
		t.Fatal("non-HTTPS destination accepted")
	}
	queue := NewDeliveryQueue(Notifier{}, 1)
	defer queue.Close()
	destination := NotificationDestination{Kind: "webhook", URL: "https://127.0.0.1/hook", Enabled: true}
	for index := 0; index < maxPendingNotifications+8; index++ {
		_ = queue.Enqueue(context.Background(), destination, event)
	}
}

func TestNotifierDoesNotLeakSecretsOrFollowRedirects(t *testing.T) {
	event := contracts.AlertHistoryEvent{ID: "event-secret", AlertID: "rule-secret", State: "firing", OccurredAt: time.Now().UTC()}
	called := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://cleartext.test/hook"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	client := &http.Client{Transport: transport}
	err := (Notifier{Client: client}).Send(context.Background(), NotificationDestination{Kind: "telegram", URL: "https://api.telegram.org", Secret: "bot-secret-token", ChatID: "123", Enabled: true}, event)
	if err == nil || called != 1 {
		t.Fatalf("redirect was followed or accepted: err=%v calls=%d", err, called)
	}
	if strings.Contains(err.Error(), "bot-secret-token") {
		t.Fatalf("notification error leaked token: %v", err)
	}
	requestErrorClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport failed")
	})}
	err = (Notifier{Client: requestErrorClient}).Send(context.Background(), NotificationDestination{Kind: "telegram", URL: "https://api.telegram.org", Secret: "another-secret-token", ChatID: "123", Enabled: true}, event)
	if err == nil || strings.Contains(err.Error(), "another-secret-token") {
		t.Fatalf("request error leaked token or was swallowed: %v", err)
	}
}

func TestDeliveryQueueReportsTerminalFailureWithoutDestinationSecrets(t *testing.T) {
	queue := NewDeliveryQueue(Notifier{}, 1)
	queue.send = func(context.Context, NotificationDestination, contracts.AlertHistoryEvent) error {
		return errors.New("terminal delivery failure")
	}
	defer queue.Close()
	event := contracts.AlertHistoryEvent{ID: "event-terminal", AlertID: "rule-terminal", ServerID: "server-alerts-0123", State: "firing", OccurredAt: time.Now().UTC()}
	if err := queue.Enqueue(context.Background(), NotificationDestination{Kind: "webhook", URL: "https://example.test/secret-path", Secret: "never-report-this", Enabled: true}, event); err != nil {
		t.Fatal(err)
	}
	select {
	case failure := <-queue.Failures():
		if failure.EventID != event.ID || failure.AlertID != event.AlertID || failure.ServerID != event.ServerID || failure.Err == nil {
			t.Fatalf("failure=%+v", failure)
		}
		if strings.Contains(failure.Err.Error(), "never-report-this") || strings.Contains(failure.Err.Error(), "secret-path") {
			t.Fatalf("failure leaked destination credentials: %v", failure.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal delivery failure was swallowed")
	}
}
