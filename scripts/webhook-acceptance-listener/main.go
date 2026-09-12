// Command webhook-acceptance-listener receives exactly one Payesh webhook on
// loopback, verifies its HMAC, reports only non-secret fields, and exits.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	secret := os.Getenv("PAYESH_ALERT_WEBHOOK_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "PAYESH_ALERT_WEBHOOK_SECRET is required")
		os.Exit(2)
	}
	failFirst := 0
	if value := strings.TrimSpace(os.Getenv("PAYESH_WEBHOOK_FAIL_FIRST")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 4 {
			fmt.Fprintln(os.Stderr, "PAYESH_WEBHOOK_FAIL_FIRST must be between 0 and 4")
			os.Exit(2)
		}
		failFirst = parsed
	}
	var attempts atomic.Int32
	result := make(chan error, 1)
	listen := strings.TrimSpace(os.Getenv("PAYESH_WEBHOOK_LISTEN"))
	if listen == "" {
		listen = "127.0.0.1:18787"
	}
	server := &http.Server{Addr: listen, ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/payesh-test-hook" {
			http.NotFound(w, request)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 65<<10))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			result <- err
			return
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		provided, err := hex.DecodeString(strings.TrimSpace(request.Header.Get("X-Payesh-Signature")))
		if err != nil || !hmac.Equal(provided, mac.Sum(nil)) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			result <- fmt.Errorf("webhook signature verification failed")
			return
		}
		var payload struct {
			AlertID string `json:"alert_id"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.AlertID == "" || payload.State == "" {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			result <- fmt.Errorf("webhook payload verification failed")
			return
		}
		attempt := int(attempts.Add(1))
		if attempt <= failFirst {
			http.Error(w, "temporary outage", http.StatusServiceUnavailable)
			fmt.Printf("webhook_attempt=%d simulated_status=503 signature=valid\n", attempt)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		fmt.Printf("webhook_receive=PASS attempts=%d alert_id=%s state=%s signature=valid\n", attempt, payload.AlertID, payload.State)
		result <- nil
	})
	go func() {
		certFile := strings.TrimSpace(os.Getenv("PAYESH_WEBHOOK_TLS_CERT"))
		keyFile := strings.TrimSpace(os.Getenv("PAYESH_WEBHOOK_TLS_KEY"))
		var err error
		if certFile != "" && keyFile != "" {
			err = server.ListenAndServeTLS(certFile, keyFile)
		} else if certFile != "" || keyFile != "" {
			err = fmt.Errorf("both PAYESH_WEBHOOK_TLS_CERT and PAYESH_WEBHOOK_TLS_KEY are required")
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			result <- err
		}
	}()
	select {
	case err := <-result:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = server.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case <-time.After(60 * time.Second):
		_ = server.Close()
		fmt.Fprintln(os.Stderr, "timed out waiting for webhook")
		os.Exit(1)
	}
}
