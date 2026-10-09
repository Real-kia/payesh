package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoAPI records the request a tool produced and answers with fixed JSON.
type echoAPI struct{ last *http.Request }

func (e *echoAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.last = r
	if r.URL.Path == "/api/v1/servers/missing-server-id" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"not_found"}`)
		return
	}
	_, _ = io.WriteString(w, `{"items":[]}`)
}

func post(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer pyt_test")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var message map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &message); err != nil {
			t.Fatalf("response is not JSON: %s", w.Body.String())
		}
	}
	return w.Code, message
}

func TestProtocolHandshakeAndErrors(t *testing.T) {
	s := &Server{API: &echoAPI{}, OpenAPI: []byte("openapi: 3.0.3"), Version: "test"}
	_, message := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	result := message["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersions[0] || result["capabilities"].(map[string]any)["resources"] == nil {
		t.Fatalf("initialize: %v", message)
	}
	if status, _ := post(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); status != http.StatusAccepted {
		t.Fatalf("notification status: %d", status)
	}
	if _, message := post(t, s, `{"jsonrpc":"2.0","id":2,"method":"nope"}`); message["error"].(map[string]any)["code"].(float64) != codeMethodNotFound {
		t.Fatalf("unknown method: %v", message)
	}
	if status, _ := post(t, s, `[{"jsonrpc":"2.0","id":3,"method":"ping"}]`); status != http.StatusBadRequest {
		t.Fatalf("batch status: %d", status)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status: %d", w.Code)
	}
}

func TestToolsListDescribesEveryTool(t *testing.T) {
	s := &Server{API: &echoAPI{}}
	_, message := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listed := message["result"].(map[string]any)["tools"].([]any)
	if len(listed) != len(tools) {
		t.Fatalf("listed %d of %d tools", len(listed), len(tools))
	}
	seen := map[string]bool{}
	for _, entry := range tools {
		if seen[entry.Name] || entry.Description == "" || entry.Title == "" {
			t.Fatalf("tool %q is duplicated or undocumented", entry.Name)
		}
		seen[entry.Name] = true
		for _, placeholder := range strings.Split(entry.Path, "{")[1:] {
			name := strings.SplitN(placeholder, "}", 2)[0]
			found := false
			for _, p := range entry.Params {
				found = found || p.Name == name && p.Required
			}
			if !found {
				t.Fatalf("tool %s path parameter %s is not a required argument", entry.Name, name)
			}
		}
	}
}

func TestToolCallsBecomeRESTRequests(t *testing.T) {
	api := &echoAPI{}
	s := &Server{API: api}
	_, message := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_logs","arguments":{"server_id":"server-0123456789abcdef","source":"journal","limit":50}}}`)
	if message["result"].(map[string]any)["isError"] != false {
		t.Fatalf("query_logs: %v", message)
	}
	query := api.last.URL.Query()
	if api.last.URL.Path != "/api/v1/servers/server-0123456789abcdef/logs" || query.Get("limit") != "50" || query.Get("source") != "journal" || api.last.Header.Get("Authorization") != "Bearer pyt_test" {
		t.Fatalf("request: %s %v", api.last.URL, api.last.Header)
	}
	from, fromErr := time.Parse(time.RFC3339, query.Get("from"))
	to, toErr := time.Parse(time.RFC3339, query.Get("to"))
	if fromErr != nil || toErr != nil || to.Sub(from) != time.Hour {
		t.Fatalf("default window: %s..%s", query.Get("from"), query.Get("to"))
	}

	_, message = post(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_server","arguments":{"server_id":"missing-server-id"}}}`)
	result := message["result"].(map[string]any)
	if result["isError"] != true || !strings.Contains(result["content"].([]any)[0].(map[string]any)["text"].(string), "HTTP 404") {
		t.Fatalf("API error is not a tool error: %v", message)
	}
}

func TestToolArgumentsAreValidated(t *testing.T) {
	cases := map[string]map[string]any{
		"get_server":   {"server_id": "../../accounts"},
		"list_servers": {"surprise": "1"},
		"query_logs":   {"server_id": "server-0123456789abcdef"},
		"get_metrics":  {"server_id": "server-0123456789abcdef", "limit": 1.5},
	}
	for name, args := range cases {
		entry, _ := findTool(name)
		if _, _, _, err := entry.build(args, time.Now()); err == nil {
			t.Fatalf("%s accepted %v", name, args)
		}
	}
	generic, _ := findTool("api_request")
	for _, args := range []map[string]any{
		{"method": "GET", "path": "/healthz"},
		{"method": "GET", "path": "/api/v1/../setup"},
		{"method": "GET", "path": "/api/v1/servers?limit=1"},
		{"method": "POST", "path": "/api/v1/mcp"},
		{"method": "TRACE", "path": "/api/v1/servers"},
		{"method": "GET", "path": "/api/v1/servers", "body": map[string]any{"x": 1}},
	} {
		if _, _, _, err := generic.build(args, time.Now()); err == nil {
			t.Fatalf("api_request accepted %v", args)
		}
	}
	method, target, body, err := generic.build(map[string]any{"method": "PATCH", "path": "/api/v1/alerts/rules/r1", "query": map[string]any{"b": "2", "a": 1.0}, "body": map[string]any{"enabled": false}}, time.Now())
	if err != nil || method != "PATCH" || target != "/api/v1/alerts/rules/r1?a=1&b=2" || !bytes.Equal(body, []byte(`{"enabled":false}`)) {
		t.Fatalf("api_request: %s %s %s %v", method, target, body, err)
	}
}

func TestToolResultsAreBounded(t *testing.T) {
	big := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxResultBytes+10))
	})
	s := &Server{API: big}
	_, message := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami"}}`)
	text := message["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasSuffix(text, "[response truncated; request a smaller page]") || len(text) > maxResultBytes+100 {
		t.Fatalf("result length %d", len(text))
	}
}
