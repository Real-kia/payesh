package transport

import (
	"context"
	"net/netip"
	"time"
)

// recordPeer remembers the node's public source address for best-effort
// country display. It never affects the connection: failures are ignored.
func (h *Hub) recordPeer(ctx context.Context, connection *Connection, ws *webSocket) {
	if h == nil || h.Store == nil || ws == nil || ws.conn == nil || connection == nil {
		return
	}
	serverID := connection.ServerID
	addrPort, err := netip.ParseAddrPort(ws.conn.RemoteAddr().String())
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
