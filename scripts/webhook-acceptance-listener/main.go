// Command webhook-acceptance-listener receives exactly one Payesh webhook on
// loopback, verifies its HMAC, reports only non-secret fields, and exits.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	secret := os.Getenv("PAYESH_ALERT_WEBHOOK_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "PAYESH_ALERT_WEBHOOK_SECRET is required")
		os.Exit(2)
	}
	result := make(chan error, 1)
	server := &http.Server{Addr: "127.0.0.1:18787", ReadHeaderTimeout: 5 * time.Second}
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
		w.WriteHeader(http.StatusNoContent)
		fmt.Printf("webhook_receive=PASS alert_id=%s state=%s signature=valid\n", payload.AlertID, payload.State)
		result <- nil
	})
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			result <- err
		}
	}()
	select {
	case err := <-result:
		_ = server.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case <-time.After(30 * time.Second):
		_ = server.Close()
		fmt.Fprintln(os.Stderr, "timed out waiting for webhook")
		os.Exit(1)
	}
}
