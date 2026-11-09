package alerts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
	"unicode/utf8"
)

// PayloadSchema versions the native Payesh JSON payload. New fields may be
// added to v1; existing fields keep their meaning.
const PayloadSchema = "payesh.alert.v1"

const maxRenderedBytes = 16 << 10

// TemplateData is the value a custom template is executed against. It holds
// only plain strings, numbers and slices, so a template cannot call methods
// or reach anything but these fields.
type TemplateData struct {
	Schema      string
	Kind        string
	State       string
	Title       string
	Summary     string
	Reason      string
	Rule        string
	RuleID      string
	Expression  string
	Metric      string
	Operator    string
	Threshold   string
	Value       string
	Server      string
	ServerID    string
	Tags        []string
	Since       string
	OccurredAt  string
	AlertID     string
	EventID     string
	IncidentID  string
	IncidentURL string
	Digest      bool
	Count       int
	Dropped     int
	Items       []TemplateData
}

func formatNumber(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func displayKind(kind string) string {
	switch kind {
	case NotifyFiring:
		return "FIRING"
	case NotifyRecovered:
		return "RECOVERED"
	case NotifyReminder:
		return "STILL FIRING"
	case NotifyTest:
		return "TEST"
	case NotifyDigest:
		return "DIGEST"
	}
	return strings.ToUpper(kind)
}

func (n Notification) ruleLabel() string {
	switch {
	case n.RuleName != "":
		return n.RuleName
	case n.RuleID != "":
		return n.RuleID
	}
	return n.Event.AlertID
}

func (n Notification) serverLabel() string {
	if n.ServerName != "" {
		return n.ServerName
	}
	return string(n.Event.ServerID)
}

// summaryLine is the one-line human description used by text-like formats.
func (n Notification) summaryLine() string {
	if n.Kind == NotifyDigest {
		return fmt.Sprintf("%d alert notifications were grouped to stay under the rate limit", len(n.Items)+n.Dropped)
	}
	var b strings.Builder
	b.WriteString("[" + displayKind(n.Kind) + "] " + n.ruleLabel())
	if server := n.serverLabel(); server != "" {
		b.WriteString(" on " + server)
	}
	if n.Expression != "" {
		b.WriteString(" (" + n.Expression + ")")
	}
	if value := formatNumber(n.Value); value != "" {
		b.WriteString(", value " + value)
	}
	if n.Kind == NotifyTest {
		return "[TEST] Payesh test notification: delivery is working"
	}
	return b.String()
}

func (n Notification) title() string {
	if n.Kind == NotifyDigest {
		return "Payesh: grouped alert notifications"
	}
	if n.Kind == NotifyTest {
		return "Payesh: test notification"
	}
	title := "Payesh " + strings.ToLower(displayKind(n.Kind)) + ": " + n.ruleLabel()
	if server := n.serverLabel(); server != "" {
		title += " on " + server
	}
	return title
}

func (n Notification) templateData() TemplateData {
	data := TemplateData{
		Schema: PayloadSchema, Kind: n.Kind, State: n.Event.State, Title: n.title(), Summary: n.summaryLine(), Reason: n.Event.Reason,
		Rule: n.ruleLabel(), RuleID: n.RuleID, Expression: n.Expression, Metric: n.Metric, Operator: n.Operator,
		Threshold: formatNumber(n.Threshold), Value: formatNumber(n.Value), Server: n.serverLabel(), ServerID: string(n.Event.ServerID),
		Tags: append([]string{}, n.Tags...), AlertID: n.Event.AlertID, EventID: n.Event.ID, IncidentID: n.Event.IncidentID,
		IncidentURL: n.IncidentURL, Digest: n.Kind == NotifyDigest, Count: 1, Dropped: n.Dropped,
	}
	if !n.Event.OccurredAt.IsZero() {
		data.OccurredAt = n.Event.OccurredAt.UTC().Format(time.RFC3339Nano)
	}
	if n.Since != nil {
		data.Since = n.Since.UTC().Format(time.RFC3339Nano)
	}
	if data.Digest {
		data.Count = len(n.Items) + n.Dropped
		for _, item := range n.Items {
			item.Items = nil
			data.Items = append(data.Items, item.templateData())
		}
	}
	return data
}

// digestText lists the folded notifications for text-like formats.
func (n Notification) digestText() string {
	var b strings.Builder
	b.WriteString(n.summaryLine())
	for _, item := range n.Items {
		b.WriteString("\n- " + item.summaryLine())
	}
	if n.Dropped > 0 {
		fmt.Fprintf(&b, "\n- ...and %d more", n.Dropped)
	}
	return b.String()
}

func (n Notification) text() string {
	if n.Kind == NotifyDigest {
		return n.digestText()
	}
	text := n.summaryLine()
	if n.IncidentURL != "" {
		text += "\n" + n.IncidentURL
	}
	return text
}

func truncateRunes(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max-1]) + "…"
}

// rendered is a ready-to-send request body.
type rendered struct {
	body        []byte
	contentType string
	headers     map[string]string
}

func nativePayload(n Notification) map[string]any {
	data := n.templateData()
	payload := map[string]any{
		"schema": PayloadSchema, "alert_id": n.Event.AlertID, "state": n.Event.State, "kind": n.Kind,
		"occurred_at": n.Event.OccurredAt.UTC().Format(time.RFC3339Nano), "reason": n.Event.Reason, "event_id": n.Event.ID,
	}
	if n.RuleID != "" || n.RuleName != "" || n.Expression != "" {
		rule := map[string]any{"id": n.RuleID, "name": n.RuleName, "expression": n.Expression}
		if n.Threshold != nil {
			rule["threshold"] = *n.Threshold
		}
		payload["rule"] = rule
	}
	if n.Event.ServerID != "" || n.ServerName != "" {
		payload["server"] = map[string]any{"id": string(n.Event.ServerID), "name": n.ServerName, "tags": data.Tags}
	}
	if n.Value != nil {
		payload["value"] = *n.Value
	}
	if n.Since != nil {
		payload["since"] = data.Since
	}
	if n.Event.IncidentID != "" || n.IncidentURL != "" {
		payload["incident"] = map[string]any{"id": n.Event.IncidentID, "url": n.IncidentURL}
	}
	if n.Kind == NotifyDigest {
		items := make([]map[string]any, 0, len(n.Items))
		for _, item := range n.Items {
			items = append(items, nativePayload(item))
		}
		payload["items"] = items
		payload["count"] = len(n.Items) + n.Dropped
		payload["dropped"] = n.Dropped
	}
	return payload
}

// render builds the request body for a destination's format.
func render(d NotificationDestination, n Notification) (rendered, error) {
	if d.Kind == KindTelegram {
		body, _ := json.Marshal(map[string]any{"chat_id": d.ChatID, "text": truncateRunes(n.text(), 4000)})
		return rendered{body: body, contentType: "application/json"}, nil
	}
	switch d.Format {
	case "", FormatPayesh:
		body, err := json.Marshal(nativePayload(n))
		return rendered{body: body, contentType: "application/json"}, err
	case FormatSlack:
		body, err := json.Marshal(map[string]any{"text": truncateRunes(n.text(), 3500)})
		return rendered{body: body, contentType: "application/json"}, err
	case FormatDiscord:
		body, err := json.Marshal(map[string]any{"content": truncateRunes(n.text(), 1900), "username": "Payesh"})
		return rendered{body: body, contentType: "application/json"}, err
	case FormatText:
		body, err := json.Marshal(map[string]any{"text": truncateRunes(n.text(), 3500)})
		return rendered{body: body, contentType: "application/json"}, err
	case FormatNtfy:
		priority, tag := "3", "bell"
		switch n.Kind {
		case NotifyFiring, NotifyReminder:
			priority, tag = "4", "rotating_light"
		case NotifyRecovered:
			priority, tag = "3", "white_check_mark"
		case NotifyTest:
			priority, tag = "2", "test_tube"
		}
		return rendered{body: []byte(truncateRunes(n.text(), 3500)), contentType: "text/plain; charset=utf-8",
			headers: map[string]string{"Title": asciiHeader(n.title()), "Priority": priority, "Tags": tag}}, nil
	case FormatTemplate:
		tpl, err := compileTemplate(d.Template)
		if err != nil {
			return rendered{}, err
		}
		var out limitedBuffer
		if err := tpl.Execute(&out, n.templateData()); err != nil {
			return rendered{}, errors.New("template could not be rendered")
		}
		body := bytes.TrimSpace(out.Bytes())
		if len(body) == 0 {
			return rendered{}, errors.New("template rendered an empty body")
		}
		contentType := "text/plain; charset=utf-8"
		if json.Valid(body) {
			contentType = "application/json"
		}
		return rendered{body: body, contentType: contentType}, nil
	}
	return rendered{}, errors.New("unknown payload format")
}

// asciiHeader makes a value safe as an HTTP header (ntfy titles are headers).
func asciiHeader(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return truncateRunes(strings.TrimSpace(b.String()), 200)
}

type limitedBuffer struct{ bytes.Buffer }

var errRenderTooLarge = errors.New("rendered body exceeds limit")

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxRenderedBytes {
		return 0, errRenderTooLarge
	}
	return b.Buffer.Write(p)
}

var templateFuncs = template.FuncMap{
	"json": func(value any) (string, error) {
		encoded, err := json.Marshal(value)
		return string(encoded), err
	},
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"join":  strings.Join,
	"truncate": func(max int, text string) string {
		if max < 1 || max > maxRenderedBytes {
			max = maxRenderedBytes
		}
		return truncateRunes(text, max)
	},
}

// compileTemplate parses an owner-supplied template. The template language is
// sandboxed: only the functions above exist, the data holds no methods, and
// {{define}}, {{template}} and {{block}} are rejected so a template cannot
// recurse. Output is capped while rendering.
func compileTemplate(text string) (*template.Template, error) {
	if len(text) == 0 || len(text) > maxTemplateBytes {
		return nil, errors.New("template must be between 1 and 4096 bytes")
	}
	tpl, err := template.New("alert").Funcs(templateFuncs).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, errors.New("template has a syntax error: " + firstLine(err.Error()))
	}
	if len(tpl.Templates()) != 1 || tpl.Tree == nil || tpl.Tree.Root == nil {
		return nil, errors.New("template must not define sub-templates")
	}
	if err := checkTemplateNodes(tpl.Tree.Root); err != nil {
		return nil, err
	}
	// Execute once against sample data so unknown fields fail at save time.
	sample := Notification{Kind: NotifyFiring, RuleName: "sample", ServerName: "sample"}.templateData()
	sample.Items = []TemplateData{sample}
	var out limitedBuffer
	if err := tpl.Execute(&out, sample); err != nil {
		return nil, errors.New("template cannot be rendered: " + firstLine(err.Error()))
	}
	return tpl, nil
}

func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return truncateRunes(text, 200)
}

func checkTemplateNodes(node parse.Node) error {
	switch n := node.(type) {
	case *parse.TemplateNode:
		return errors.New("template must not use {{template}} or {{block}}")
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, child := range n.Nodes {
			if err := checkTemplateNodes(child); err != nil {
				return err
			}
		}
	case *parse.IfNode:
		return checkBranch(&n.BranchNode)
	case *parse.RangeNode:
		// Newer Go versions can range over an integer; a literal count would
		// let a template spin the delivery worker without producing output.
		if n.Pipe != nil {
			for _, cmd := range n.Pipe.Cmds {
				for _, arg := range cmd.Args {
					if _, numeric := arg.(*parse.NumberNode); numeric {
						return errors.New("template must not range over a number")
					}
				}
			}
		}
		return checkBranch(&n.BranchNode)
	case *parse.WithNode:
		return checkBranch(&n.BranchNode)
	}
	return nil
}

func checkBranch(b *parse.BranchNode) error {
	if err := checkTemplateNodes(b.List); err != nil {
		return err
	}
	if b.ElseList != nil {
		return checkTemplateNodes(b.ElseList)
	}
	return nil
}
