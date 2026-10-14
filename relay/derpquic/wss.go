package derpquic

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"tailscale.com/net/wsconn"
)

const WSSSubprotocol = "paperboat-derp-v1"

// WSSHandler accepts the TCP reachability leg into the same authenticated
// routing, fencing, accounting, and drain owner used by DERP/QUIC.
func (s *Server) WSSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || strings.ToLower(r.Header.Get("Upgrade")) != "websocket" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			http.Error(w, "relay authorization required", http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		full := s.draining || len(s.connections)+len(s.wssConnections) >= s.connectionLimit
		s.mu.Unlock()
		if full {
			s.denied.Add(1)
			http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{WSSSubprotocol}, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		if c.Subprotocol() != WSSSubprotocol {
			_ = c.Close(websocket.StatusPolicyViolation, "relay protocol required")
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		c.SetReadLimit(MaxControl + 5)
		nc := wsconn.NetConn(ctx, c, websocket.MessageBinary, r.RemoteAddr)
		p := &peerConn{stream: nc, ctx: ctx, cancel: cancel, tlsState: *r.TLS, out: make(chan queued, queueDepth), control: make(chan []byte, 8)}
		var closeOnce sync.Once
		p.closeFn = func(code uint64, reason string) {
			closeOnce.Do(func() {
				status := websocket.StatusNormalClosure
				if code != 0 {
					status = websocket.StatusCode(4000 + code)
				}
				go func() { _ = c.Close(status, reason); cancel() }()
			})
		}
		s.mu.Lock()
		if s.draining || len(s.connections)+len(s.wssConnections) >= s.connectionLimit {
			s.mu.Unlock()
			p.close(3, "relay unavailable")
			return
		}
		s.wssConnections[p] = struct{}{}
		s.workers.Add(1)
		s.mu.Unlock()
		defer s.workers.Done()
		s.servePeer(p)
	})
}
