package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// Notifier delivers notifications over HTTP. The zero value is ready to use
// and applies the SSRF guard; tests may inject Client to avoid the network.
type Notifier struct {
	Client *http.Client
}

// DeliveryResult is the secret-free outcome of one delivery, including
// retries.
type DeliveryResult struct {
	OK         bool
	StatusCode int
	Attempts   int
	// Error is a short classification such as "timeout" or "HTTP 500". It
	// never contains the destination URL, headers, or token.
	Error    string
	Duration time.Duration
	err      error
}

// Err returns the failure as an error, or nil on success.
func (r DeliveryResult) Err() error {
	if r.OK {
		return nil
	}
	if r.err != nil {
		return r.err
	}
	return errors.New(r.Error)
}

// Status is a short label for the destination list.
func (r DeliveryResult) Status() string {
	switch {
	case r.OK && r.StatusCode > 0:
		return "HTTP " + strconv.Itoa(r.StatusCode)
	case r.OK:
		return "ok"
	case r.StatusCode > 0:
		return "HTTP " + strconv.Itoa(r.StatusCode)
	}
	return "failed"
}

// Send makes a single delivery attempt of a bare event. It is kept for
// callers that have no rule or server context.
func (n Notifier) Send(ctx context.Context, destination NotificationDestination, event contracts.AlertHistoryEvent) error {
	_, err := n.attempt(ctx, destination.withDefaults(), Notification{Event: event, Kind: kindFromEvent(event)})
	return err
}

// SendWithRetry delivers a bare event with the destination's retry policy.
func (n Notifier) SendWithRetry(ctx context.Context, destination NotificationDestination, event contracts.AlertHistoryEvent) error {
	return n.Deliver(ctx, destination, Notification{Event: event, Kind: kindFromEvent(event)}).Err()
}

func kindFromEvent(event contracts.AlertHistoryEvent) string {
	if event.State == "recovered" {
		return NotifyRecovered
	}
	return NotifyFiring
}

// Deliver sends a notification with the destination's retry policy. Only
// transient failures (network errors, 408, 429, 5xx) are retried, with
// exponential backoff capped at 30 seconds.
func (n Notifier) Deliver(ctx context.Context, destination NotificationDestination, notification Notification) DeliveryResult {
	if ctx == nil {
		ctx = context.Background()
	}
	destination = destination.withDefaults()
	started := time.Now()
	result := DeliveryResult{}
	for attempt := 1; attempt <= destination.MaxAttempts; attempt++ {
		if attempt > 1 {
			delay := destination.RetryBackoff << uint(attempt-2)
			if delay > MaxRetryBackoff || delay <= 0 {
				delay = MaxRetryBackoff
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				result.Error, result.err = "cancelled", ctx.Err()
				result.Duration = time.Since(started)
				return result
			case <-timer.C:
			}
		}
		status, err := n.attempt(ctx, destination, notification)
		result.Attempts, result.StatusCode = attempt, status
		if err == nil {
			result.OK, result.Error, result.err = true, "", nil
			result.Duration = time.Since(started)
			return result
		}
		result.Error, result.err = err.Error(), err
		if !retryable(status, err) {
			break
		}
	}
	result.Duration = time.Since(started)
	return result
}

func retryable(status int, err error) bool {
	if errors.Is(err, errBlockedAddress) || errors.Is(err, errPermanent) {
		return false
	}
	switch {
	case status == 0:
		return true
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return true
	}
	return false
}

var errPermanent = errors.New("permanent delivery error")

func permanent(message string) error { return fmt.Errorf("%s: %w", message, errPermanent) }

// attempt makes one HTTP request and returns the status code (0 when no
// response arrived) and a secret-free error.
func (n Notifier) attempt(ctx context.Context, d NotificationDestination, notification Notification) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !d.Enabled || d.URL == "" || (d.Kind != KindWebhook && d.Kind != KindTelegram) {
		return 0, permanent("notification destination is invalid or disabled")
	}
	if len(d.Secret) > 256 {
		return 0, permanent("notification secret is too long")
	}
	if err := validateDestinationURL(d.URL, d.AllowInsecureHTTP, d.AllowPrivateNetwork); err != nil {
		return 0, fmt.Errorf("%s: %w", err.Error(), errPermanent)
	}
	endpoint := d.URL
	if d.Kind == KindTelegram {
		if d.Secret == "" || d.ChatID == "" || len(d.ChatID) > 128 {
			return 0, permanent("telegram destination token and chat id are required")
		}
		endpoint = strings.TrimRight(d.URL, "/") + "/bot" + url.PathEscape(d.Secret) + "/sendMessage"
	}
	if len(endpoint) > 2048 {
		return 0, permanent("notification endpoint is too long")
	}
	body, err := render(d, notification)
	if err != nil {
		return 0, permanent(err.Error())
	}
	if len(body.body) > MaxNotificationBody {
		return 0, permanent("notification body exceeds limit")
	}
	requestContext, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body.body))
	if err != nil {
		return 0, permanent("notification request could not be created")
	}
	request.Header.Set("Content-Type", body.contentType)
	request.Header.Set("User-Agent", "payesh-alerts/1")
	request.Header.Set("X-Payesh-Event", notification.Kind)
	if notification.Event.ID != "" {
		request.Header.Set("X-Payesh-Delivery", notification.Event.ID)
	}
	for name, value := range d.Headers {
		request.Header.Set(name, value)
	}
	for name, value := range body.headers {
		request.Header.Set(name, value)
	}
	if d.Kind == KindWebhook && d.Secret != "" {
		mac := hmac.New(sha256.New, []byte(d.Secret))
		_, _ = mac.Write(body.body)
		request.Header.Set("X-Payesh-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	client := n.Client
	if client == nil {
		client = guardedClient(d.AllowPrivateNetwork, request.URL.Scheme == "http", d.Timeout)
	} else {
		// Never let net/http follow a redirect to an unvalidated URL; clone so
		// the caller's client is not mutated.
		clone := *client
		client = &clone
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	response, err := client.Do(request)
	if err != nil {
		// net/http wraps failures with the request URL, which can contain a
		// token (Telegram, Slack). Report only a classification.
		return 0, classifyNetworkError(err)
	}
	defer response.Body.Close()
	if _, readErr := ioReadLimit(response.Body, MaxNotificationBody); readErr != nil {
		return response.StatusCode, permanent("notification response exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("notification endpoint returned HTTP %d", response.StatusCode)
	}
	return response.StatusCode, nil
}

// classifyNetworkError maps a transport error to a short, secret-free reason.
func classifyNetworkError(err error) error {
	var dnsErr *net.DNSError
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	switch {
	case errors.Is(err, errBlockedAddress):
		return fmt.Errorf("destination address is blocked (private, loopback, link-local or metadata address): %w", errBlockedAddress)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("timeout")
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled")
	case errors.As(err, &dnsErr):
		return errors.New("DNS lookup failed")
	case errors.As(err, &certErr), errors.As(err, &hostErr), errors.As(err, &invalidErr):
		return errors.New("TLS certificate was not accepted")
	case errors.As(err, &recordErr):
		return errors.New("destination did not speak TLS")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errors.New("timeout")
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "connection refused"):
		return errors.New("connection refused")
	case strings.Contains(text, "no route to host"), strings.Contains(text, "network is unreachable"):
		return errors.New("network unreachable")
	case strings.Contains(text, "connection reset"), strings.Contains(text, "EOF"):
		return errors.New("connection closed by destination")
	}
	return errors.New("notification request failed")
}
