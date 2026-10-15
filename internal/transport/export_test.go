package transport

import (
	"context"
	"os"
)

// Test seams for the external transport_test package, which also needs the
// hub side (package nodehub imports this package, so an in-package test could not).

func (a *AgentClient) Migrate(ctx context.Context, ws *WebSocket, target string) error {
	return a.migrate(ctx, ws, target)
}
func (a *AgentClient) PreviousURL() string                    { return a.previousURL }
func (a *AgentClient) SetConfiguredURL(url string)            { a.configuredURL = url }
func (a *AgentClient) SetEndpointSync(f func(*os.File) error) { a.endpointSync = f }
