package mesh

import (
	"context"
	"errors"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// Ping measures one round trip to peer inside its WireGuard session, over the path its data uses: the peer's engine
// answers, so a held connection cannot (tailscale ping --tsmp, ~/ref/tailscale wgengine/userspace.go sendTSMPPing).
func (s *Server) Ping(ctx context.Context, peer key.NodePublic) (time.Duration, error) {
	s.lb.mu.Lock()
	addr, ok := s.lb.allowedPeers[peer]
	s.lb.mu.Unlock()
	if !ok {
		return 0, errors.New("mesh: ping of a peer that is not allowed")
	}
	answer := make(chan *ipnstate.PingResult, 1)
	s.lb.sys.Engine.Get().Ping(addr, tailcfg.PingTSMP, 0, func(r *ipnstate.PingResult) {
		select {
		case answer <- r:
		default:
		}
	})
	select {
	case r := <-answer:
		if r.Err != "" {
			return 0, errors.New("mesh: ping: " + r.Err)
		}
		return time.Duration(r.LatencySeconds * float64(time.Second)), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
