package nodehub

import (
	"context"
	"net/netip"
	"time"

	"github.com/Real-kia/payesh/internal/transport"
)

// recordPeer remembers the node's public source address for best-effort
// country display. It never affects the connection: failures are ignored.
func (h *Hub) recordPeer(ctx context.Context, connection *Connection, ws *transport.WebSocket) {
	if h == nil || h.Store == nil || ws == nil || ws.RemoteAddr() == nil || connection == nil {
		return
	}
	serverID := connection.ServerID
	addrPort, err := netip.ParseAddrPort(ws.RemoteAddr().String())
	if err != nil {
		return
	}
	ip := addrPort.Addr().Unmap()
	connection.stateMu.Lock()
	connection.peerIP = ip.String()
	connection.stateMu.Unlock()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = h.Store.RecordServerPeerIP(writeCtx, serverID, ip)
}
