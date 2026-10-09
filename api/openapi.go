// Package api embeds the canonical OpenAPI description of the REST API so the
// server can hand it to API and MCP clients.
package api

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
