package nodehub

import (
	"net"

	"github.com/Real-kia/payesh/internal/transport"
)

func (p *NodePorts) migrationURL(c *Connection) string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	supported := false
	for _, cap := range c.hello.Capabilities {
		if cap == transport.MigrationCapability {
			supported = true
		}
	}
	if !supported || c.closed {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Nodes[c.ServerID].Port == p.currentLocked() {
		return ""
	}
	return p.publicURL(p.currentLocked())
}
func localPort(ws *transport.WebSocket) string {
	_, port, _ := net.SplitHostPort(ws.LocalAddr().String())
	return port
}
