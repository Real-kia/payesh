// Command notification-acceptance sends one bounded test event through the
// same notifier used by payesh-server. Credentials are read only from the
// environment and are never printed.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
)

func main() {
	event := contracts.AlertHistoryEvent{ID: "notification-acceptance", AlertID: "payesh-test", State: "firing", OccurredAt: time.Now().UTC(), Reason: "notification acceptance test"}
	destinations := []alerts.NotificationDestination{}
	if endpoint := strings.TrimSpace(os.Getenv("PAYESH_ALERT_WEBHOOK_URL")); endpoint != "" {
		destinations = append(destinations, alerts.NotificationDestination{Kind: "webhook", URL: endpoint, Secret: os.Getenv("PAYESH_ALERT_WEBHOOK_SECRET"), Enabled: true})
	}
	if token := strings.TrimSpace(os.Getenv("PAYESH_ALERT_TELEGRAM_TOKEN")); token != "" {
		apiURL := strings.TrimSpace(os.Getenv("PAYESH_ALERT_TELEGRAM_API_URL"))
		if apiURL == "" {
			apiURL = "https://api.telegram.org"
		}
		destinations = append(destinations, alerts.NotificationDestination{Kind: "telegram", URL: apiURL, Secret: token, ChatID: strings.TrimSpace(os.Getenv("PAYESH_ALERT_TELEGRAM_CHAT_ID")), Enabled: true})
	}
	if len(destinations) == 0 {
		fmt.Fprintln(os.Stderr, "no notification destination configured")
		os.Exit(2)
	}
	for _, destination := range destinations {
		timeout := 20 * time.Second
		if os.Getenv("PAYESH_NOTIFICATION_RETRY") == "1" {
			// Five notifier attempts can each consume ten seconds, with bounded
			// 1s+2s+4s+8s backoff between them. Leave a small scheduling margin.
			timeout = 70 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		var err error
		if os.Getenv("PAYESH_NOTIFICATION_RETRY") == "1" {
			err = (alerts.Notifier{}).SendWithRetry(ctx, destination, event)
		} else {
			err = (alerts.Notifier{}).Send(ctx, destination, event)
		}
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s notification failed: %v\n", destination.Kind, err)
			os.Exit(1)
		}
		fmt.Printf("%s notification=PASS\n", destination.Kind)
	}
}
