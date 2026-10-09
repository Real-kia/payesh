package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxResultBytes bounds what one tool call returns to the model. The REST API
// already caps responses at 1 MiB; this keeps a misbehaving route from
// flooding the agent's context.
const maxResultBytes = 1 << 20

// tool maps one MCP tool onto one REST route. Adding a tool for an existing
// endpoint only needs a new entry in tools.
type tool struct {
	Name        string
	Title       string
	Description string
	Method      string
	// Path uses {name} placeholders that are filled from arguments of the
	// same name. All other arguments become query parameters.
	Path   string
	Params []param
	// RecentWindow fills omitted from/to arguments with the window ending now,
	// for routes that require a time range.
	RecentWindow time.Duration
}

type param struct {
	Name        string
	Type        string // "string", "integer", or "object"
	Description string
	Required    bool
	Enum        []string
}

var (
	serverID   = param{Name: "server_id", Type: "string", Required: true, Description: "Server ID from list_servers."}
	limit      = param{Name: "limit", Type: "integer", Description: "Maximum items to return (1..200)."}
	cursor     = param{Name: "cursor", Type: "string", Description: "next_cursor from a previous page."}
	from       = param{Name: "from", Type: "string", Description: "Range start, RFC3339 UTC. Defaults to the recent window."}
	to         = param{Name: "to", Type: "string", Description: "Range end, RFC3339 UTC, at most 31 days after from. Defaults to now."}
	optionalTo = param{Name: "to", Type: "string", Description: "Range end, RFC3339 UTC."}
)

var tools = []tool{
	{Name: "whoami", Title: "Current account", Method: http.MethodGet, Path: "/api/v1/account/me",
		Description: "Show the account and permission (read or edit) this token acts as."},
	{Name: "list_servers", Title: "List servers", Method: http.MethodGet, Path: "/api/v1/servers", Params: []param{limit, cursor},
		Description: "List monitored servers with role, platform, connection state, and data freshness."},
	{Name: "get_server", Title: "Get server", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}", Params: []param{serverID},
		Description: "Get one server's details, capabilities, and connection and freshness state."},
	{Name: "get_metrics", Title: "Query metrics", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}/metrics", RecentWindow: time.Hour,
		Params: []param{serverID, from, to,
			{Name: "resolution", Type: "string", Enum: []string{"raw", "minute", "hour"}, Description: "raw samples (default) or minute/hour rollups. Use rollups for ranges longer than a few hours."},
			limit, cursor, {Name: "gaps_cursor", Type: "string", Description: "gaps_next_cursor from a previous raw page."}},
		Description: "Query CPU, memory, disk, load, and network metrics for a server. Defaults to the last hour. Coverage gaps are reported separately and are not zero values."},
	{Name: "list_processes", Title: "List processes", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}/processes",
		Params: []param{serverID, {Name: "sort", Type: "string", Enum: []string{"cpu", "memory", "read", "write", "connections", "pid"}, Description: "Sort key, default cpu."},
			{Name: "search", Type: "string", Description: "Case-insensitive filter on process name or command."},
			{Name: "limit", Type: "integer", Description: "Maximum processes to return (1..1000, default 200)."}},
		Description: "List the server's current processes with CPU, memory, disk I/O, and connection counts. Needs the process monitoring package."},
	{Name: "get_traffic_periods", Title: "Traffic periods", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}/traffic", RecentWindow: 30 * 24 * time.Hour,
		Params:      []param{serverID, from, to, {Name: "scope", Type: "string", Description: "Traffic scope to filter by."}, limit, cursor},
		Description: "Get traffic allowance periods: counted bytes, allowance, direction, and continuity. Defaults to the last 30 days. Totals are host-side estimates; gap or uncertain continuity means some traffic may be missing."},
	{Name: "get_traffic_forecast", Title: "Traffic forecast", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}/traffic/forecast",
		Params: []param{serverID, {Name: "scope", Type: "string", Required: true, Description: "Traffic scope, from get_traffic_periods."},
			{Name: "direction", Type: "string", Required: true, Enum: []string{"inbound", "outbound", "combined"}, Description: "Counted direction."},
			{Name: "as_of", Type: "string", Description: "Past RFC3339 instant to forecast from. Defaults to now."}},
		Description: "Estimate end-of-period traffic from the recent rate. Returns available: false with a reason when there is less than 24 hours of usable data."},
	{Name: "list_log_sources", Title: "List log sources", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}/logs/sources", Params: []param{serverID, limit, cursor},
		Description: "List the log sources (journal units and files) collected for a server. Use a source ID with query_logs."},
	{Name: "query_logs", Title: "Query logs", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}/logs", RecentWindow: time.Hour,
		Params: []param{serverID, {Name: "source", Type: "string", Required: true, Description: "Log source ID from list_log_sources."},
			from, to, {Name: "severity", Type: "string", Description: "Only entries with this severity, for example err or warning."},
			{Name: "search", Type: "string", Description: "Text to search for in log messages."},
			{Name: "order", Type: "string", Enum: []string{"desc", "asc"}, Description: "desc (newest first, default) or asc."}, limit, cursor},
		Description: "Search stored log entries for one source. Defaults to the last hour, newest first."},
	{Name: "list_active_alerts", Title: "Active alerts", Method: http.MethodGet, Path: "/api/v1/alerts",
		Params:      []param{{Name: "server_id", Type: "string", Description: "Only alerts for this server."}, limit, cursor},
		Description: "List current alert states, including firing alerts."},
	{Name: "get_alert_history", Title: "Alert history", Method: http.MethodGet, Path: "/api/v1/alerts/history",
		Params:      []param{{Name: "from", Type: "string", Description: "Range start, RFC3339 UTC."}, optionalTo, limit, cursor},
		Description: "List past alert transitions and incidents."},
	{Name: "get_incident", Title: "Get incident", Method: http.MethodGet, Path: "/api/v1/incidents/{incident_id}",
		Params:      []param{{Name: "incident_id", Type: "string", Required: true, Description: "Incident ID from alerts or alert history."}},
		Description: "Get one incident with the metrics, logs, and processes captured around it."},
	{Name: "list_alert_rules", Title: "Alert rules", Method: http.MethodGet, Path: "/api/v1/alerts/rules", Params: []param{limit, cursor},
		Description: "List configured alert rules and their thresholds."},
	{Name: "list_maintenance_windows", Title: "Maintenance windows", Method: http.MethodGet, Path: "/api/v1/maintenance-windows", Params: []param{limit, cursor},
		Description: "List maintenance windows that silence alerts."},
	{Name: "list_packages", Title: "Package catalog", Method: http.MethodGet, Path: "/api/v1/modules",
		Description: "List optional packages available to install, such as process monitoring, CPU controls, bandwidth limits, and port traffic."},
	{Name: "list_server_packages", Title: "Server packages", Method: http.MethodGet, Path: "/api/v1/servers/{server_id}/modules", Params: []param{serverID},
		Description: "List the packages installed on a server and their state and revision."},
	{Name: "get_job", Title: "Get job", Method: http.MethodGet, Path: "/api/v1/jobs/{job_id}",
		Params:      []param{{Name: "job_id", Type: "string", Required: true, Description: "Job ID returned by a mutation."}},
		Description: "Get the progress and outcome of a background job such as an installation or update."},
	{Name: "get_update_status", Title: "Update status", Method: http.MethodGet, Path: "/api/v1/updates/status",
		Description: "Show the running Payesh version and the state of any update in progress."},
	{Name: "api_request", Title: "Call the REST API",
		Params: []param{
			{Name: "method", Type: "string", Required: true, Enum: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}, Description: "HTTP method."},
			{Name: "path", Type: "string", Required: true, Description: "Path under /api/v1, for example /api/v1/servers/{id}/traffic. See the payesh://openapi.yaml resource."},
			{Name: "query", Type: "object", Description: "Query parameters as string values."},
			{Name: "body", Type: "object", Description: "JSON request body for POST, PUT, and PATCH."}},
		Description: "Call any Payesh REST endpoint, for operations without a dedicated tool such as configuring traffic allowances, alert rules, or packages. Mutations need an edit token and are real changes to the servers: read the current state first and pass its expected_revision plus a fresh idempotency_key."},
}

func findTool(name string) (tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

func (t tool) readOnly() bool { return t.Method == http.MethodGet }

func toolDescriptors() []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		properties := map[string]any{}
		required := []string{}
		for _, p := range t.Params {
			schema := map[string]any{"type": p.Type, "description": p.Description}
			if len(p.Enum) > 0 {
				schema["enum"] = p.Enum
			}
			if p.Type == "object" {
				schema["additionalProperties"] = true
			}
			properties[p.Name] = schema
			if p.Required {
				required = append(required, p.Name)
			}
		}
		input := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			input["required"] = required
		}
		annotations := map[string]any{"title": t.Title, "readOnlyHint": t.readOnly(), "openWorldHint": false}
		if !t.readOnly() {
			annotations["destructiveHint"] = true
		}
		result = append(result, map[string]any{"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": input, "annotations": annotations})
	}
	return result
}

var pathValuePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// build turns tool arguments into a REST method, request target, and body.
func (t tool) build(args map[string]any, now time.Time) (string, string, []byte, error) {
	if t.Name == "api_request" {
		return buildAPIRequest(args)
	}
	known := map[string]param{}
	for _, p := range t.Params {
		known[p.Name] = p
	}
	for name := range args {
		if _, ok := known[name]; !ok {
			return "", "", nil, fmt.Errorf("%w: unknown argument %q", errInvalidArgument, name)
		}
	}
	values := map[string]string{}
	for _, p := range t.Params {
		raw, present := args[p.Name]
		if !present || raw == nil {
			if p.Required {
				return "", "", nil, fmt.Errorf("%w: %s is required", errInvalidArgument, p.Name)
			}
			continue
		}
		value, err := scalar(raw)
		if err != nil {
			return "", "", nil, fmt.Errorf("%w: %s %v", errInvalidArgument, p.Name, err)
		}
		values[p.Name] = value
	}
	if t.RecentWindow > 0 {
		end := now.UTC().Truncate(time.Second)
		if _, ok := values["to"]; !ok {
			values["to"] = end.Format(time.RFC3339)
		}
		if _, ok := values["from"]; !ok {
			if parsed, err := time.Parse(time.RFC3339Nano, values["to"]); err == nil {
				end = parsed
			}
			values["from"] = end.Add(-t.RecentWindow).UTC().Format(time.RFC3339)
		}
	}
	path := t.Path
	for name, value := range values {
		placeholder := "{" + name + "}"
		if !strings.Contains(path, placeholder) {
			continue
		}
		if !pathValuePattern.MatchString(value) {
			return "", "", nil, fmt.Errorf("%w: %s has an invalid format", errInvalidArgument, name)
		}
		path = strings.ReplaceAll(path, placeholder, url.PathEscape(value))
		delete(values, name)
	}
	query := url.Values{}
	for name, value := range values {
		query.Set(name, value)
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return t.Method, path, nil, nil
}

func buildAPIRequest(args map[string]any) (string, string, []byte, error) {
	for name := range args {
		if name != "method" && name != "path" && name != "query" && name != "body" {
			return "", "", nil, fmt.Errorf("%w: unknown argument %q", errInvalidArgument, name)
		}
	}
	method, _ := args["method"].(string)
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", "", nil, fmt.Errorf("%w: method must be GET, POST, PUT, PATCH, or DELETE", errInvalidArgument)
	}
	path, _ := args["path"].(string)
	if !strings.HasPrefix(path, "/api/v1/") || strings.ContainsAny(path, "?#\\") || strings.Contains(path, "..") || strings.Contains(path, "//") {
		return "", "", nil, fmt.Errorf("%w: path must be an /api/v1/ path without a query string", errInvalidArgument)
	}
	if path == "/api/v1/mcp" {
		return "", "", nil, fmt.Errorf("%w: the MCP endpoint cannot call itself", errInvalidArgument)
	}
	target := path
	if raw, ok := args["query"]; ok && raw != nil {
		object, ok := raw.(map[string]any)
		if !ok {
			return "", "", nil, fmt.Errorf("%w: query must be an object", errInvalidArgument)
		}
		query := url.Values{}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, err := scalar(object[key])
			if err != nil {
				return "", "", nil, fmt.Errorf("%w: query %s %v", errInvalidArgument, key, err)
			}
			query.Set(key, value)
		}
		if encoded := query.Encode(); encoded != "" {
			target += "?" + encoded
		}
	}
	var body []byte
	if raw, ok := args["body"]; ok && raw != nil {
		if method == http.MethodGet {
			return "", "", nil, fmt.Errorf("%w: GET requests take no body", errInvalidArgument)
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			return "", "", nil, fmt.Errorf("%w: body is not valid JSON", errInvalidArgument)
		}
		body = encoded
	}
	return method, target, body, nil
}

func scalar(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > 1<<53 {
			return "", fmt.Errorf("must be an integer")
		}
		return strconv.FormatInt(int64(v), 10), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return "", fmt.Errorf("must be a string or number")
}

// callTool replays the tool as an in-process REST request with the caller's
// credentials and returns the API response to the model unchanged.
func (s *Server) callTool(r *http.Request, t tool, args map[string]any) map[string]any {
	method, target, body, err := t.build(args, time.Now())
	if err != nil {
		return toolResult(err.Error(), true)
	}
	inner, err := http.NewRequestWithContext(r.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		return toolResult("invalid request: "+err.Error(), true)
	}
	inner.Header.Set("Authorization", r.Header.Get("Authorization"))
	inner.Header.Set("Accept", "application/json")
	if body != nil {
		inner.Header.Set("Content-Type", "application/json")
	}
	// Keep the connection facts the API uses for transport checks, such as
	// HTTPS reported by a trusted reverse proxy.
	inner.RemoteAddr, inner.Host, inner.TLS = r.RemoteAddr, r.Host, r.TLS
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Proto"} {
		if values := r.Header.Values(name); len(values) > 0 {
			inner.Header[name] = values
		}
	}
	recorder := &recorder{header: http.Header{}}
	s.API.ServeHTTP(recorder, inner)
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	text := strings.TrimSpace(recorder.body.String())
	if recorder.truncated {
		text += "\n[response truncated; request a smaller page]"
	}
	if status >= 400 {
		return toolResult(fmt.Sprintf("HTTP %d %s: %s", status, http.StatusText(status), text), true)
	}
	if text == "" {
		text = fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
	}
	return toolResult(text, false)
}

// recorder is a bounded in-memory http.ResponseWriter for tool dispatch.
type recorder struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	truncated bool
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *recorder) Write(data []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if room := maxResultBytes - r.body.Len(); len(data) > room {
		r.body.Write(data[:max(room, 0)])
		r.truncated = true
		return len(data), nil
	}
	return r.body.Write(data)
}
func (r *recorder) Flush() {}
