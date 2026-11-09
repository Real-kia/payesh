package alerts

import (
	"context"
	"strings"

	"github.com/Real-kia/payesh/internal/contracts"
)

// buildNotification gathers the rule and server context a destination's
// payload and filters need. Lookups are best-effort: a missing server record
// still produces a deliverable notification.
func (e *Engine) buildNotification(ctx context.Context, rule contracts.AlertRule, state contracts.AlertState, event contracts.AlertHistoryEvent, kind string) Notification {
	notification := Notification{Event: event, Kind: kind, RuleID: rule.ID, RuleName: rule.Name, Expression: rule.Expression, Value: event.Value, Since: state.FiringSince}
	if notification.Value == nil {
		notification.Value = state.LastValue
	}
	if metric, operator, threshold, err := ParseExpression(rule.Expression); err == nil {
		notification.Metric, notification.Operator = metric, operator
		value := threshold
		notification.Threshold = &value
	}
	if event.ServerID != "" && e.Store != nil {
		if server, found, err := e.Store.GetServer(ctx, event.ServerID); err == nil && found {
			notification.ServerName = server.Name
			notification.Tags = append([]string(nil), server.Tags...)
		}
	}
	if base := strings.TrimRight(strings.TrimSpace(e.PublicURL), "/"); base != "" && event.IncidentID != "" {
		notification.IncidentURL = base + "/alerts?incident=" + event.IncidentID
	}
	return notification
}

// filterDestinations keeps the enabled destinations whose routing accepts the
// notification. Destinations with no filter receive everything.
func filterDestinations(destinations []NotificationDestination, notification Notification) []NotificationDestination {
	kept := make([]NotificationDestination, 0, len(destinations))
	for _, destination := range destinations {
		if destination.Filter.Matches(notification) {
			kept = append(kept, destination)
		}
	}
	return kept
}
