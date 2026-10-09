// Package mcp exposes the Payesh REST API to AI agents over the Model Context
// Protocol (Streamable HTTP transport, stateless JSON responses).
//
// The package holds no business logic and makes no authorization decisions.
// Every tool call is replayed as an ordinary in-process REST request carrying
// the caller's own credentials, so an agent can do exactly what its API token
// can do through the REST API, nothing more.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
)

const maxMessageBytes = 1 << 20

// ProtocolVersions lists the MCP revisions this server speaks, newest first.
var ProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const instructions = `Payesh monitors Linux servers: metrics, network traffic allowances, logs, processes, alerts, and optional packages.
Start with list_servers to learn server IDs. Time ranges are RFC3339 UTC and at most 31 days; omitted ranges default to a recent window.
Collections return at most 200 items; pass the returned next_cursor as cursor to continue.
For anything without a dedicated tool, read the payesh://openapi.yaml resource and use api_request.
Mutations need an edit token and usually an idempotency_key plus the resource's current expected_revision; read the resource first.`

// Server answers MCP requests by dispatching tool calls to API.
type Server struct {
	// API is the complete REST handler. Tool calls are sent to it with the
	// incoming request's Authorization header.
	API http.Handler
	// OpenAPI is served as the payesh://openapi.yaml resource when non-empty.
	OpenAPI []byte
	// Version is reported to clients as the server implementation version.
	Version string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// This server never opens a server-to-client event stream.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !slices.Contains(ProtocolVersions, version) {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessageBytes))
	if err != nil {
		writeMessage(w, http.StatusRequestEntityTooLarge, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest, "message too large"}})
		return
	}
	var message request
	if err := json.Unmarshal(body, &message); err != nil || len(bytes.TrimSpace(body)) == 0 || body[0] == '[' {
		writeMessage(w, http.StatusBadRequest, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParseError, "expected a single JSON-RPC message"}})
		return
	}
	// Notifications and client responses carry no reply.
	if len(message.ID) == 0 || message.Method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := s.dispatch(r, message)
	writeMessage(w, http.StatusOK, response{JSONRPC: "2.0", ID: message.ID, Result: result, Error: rpcErr})
}

func (s *Server) dispatch(r *http.Request, message request) (any, *rpcError) {
	if message.JSONRPC != "2.0" {
		return nil, &rpcError{codeInvalidRequest, "jsonrpc must be 2.0"}
	}
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(message.Params, &params)
		version := ProtocolVersions[0]
		if slices.Contains(ProtocolVersions, params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		capabilities := map[string]any{"tools": map[string]any{}}
		if len(s.OpenAPI) > 0 {
			capabilities["resources"] = map[string]any{}
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    capabilities,
			"serverInfo":      map[string]any{"name": "payesh", "title": "Payesh", "version": s.Version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDescriptors()}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return nil, &rpcError{codeInvalidParams, "invalid tools/call parameters"}
		}
		tool, ok := findTool(params.Name)
		if !ok {
			return nil, &rpcError{codeInvalidParams, "unknown tool: " + params.Name}
		}
		return s.callTool(r, tool, params.Arguments), nil
	case "resources/list":
		resources := []map[string]any{}
		if len(s.OpenAPI) > 0 {
			resources = append(resources, map[string]any{"uri": openAPIResource, "name": "openapi.yaml", "title": "Payesh REST API (OpenAPI 3)", "mimeType": "application/yaml"})
		}
		return map[string]any{"resources": resources}, nil
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil || params.URI != openAPIResource || len(s.OpenAPI) == 0 {
			return nil, &rpcError{-32002, "resource not found"}
		}
		return map[string]any{"contents": []map[string]any{{"uri": openAPIResource, "mimeType": "application/yaml", "text": string(s.OpenAPI)}}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	}
	return nil, &rpcError{codeMethodNotFound, "method not found: " + message.Method}
}

const openAPIResource = "payesh://openapi.yaml"

func writeMessage(w http.ResponseWriter, status int, message response) {
	data, err := json.Marshal(message)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// toolResult builds a tools/call result. API failures are reported as tool
// errors, not protocol errors, so the agent can read and correct them.
func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

var errInvalidArgument = errors.New("invalid argument")
