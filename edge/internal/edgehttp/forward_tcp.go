package edgehttp

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/forward"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

var errNoPublicTCPTunnel = errors.New("this node holds no public TCP tunnel for the port")

// forwardTCP passes a visitor of a port this node listens on only to forward, to the node holding the tunnel. The
// forwarder takes no ingress lease and records no traffic: the holder meters the visitor's bytes once, on its
// ForwardPublicTCP path. With no holder the connection closes, as before forwarding existed.
func (p *PublicTCPListeners) forwardTCP(ctx context.Context, conn net.Conn, port uint16, hooks *ForwardHooks) {
	defer conn.Close()
	if hooks == nil || hooks.Forwarder == nil {
		return
	}
	keepPublicTCPAlive(conn)
	if err := hooks.Forwarder.Forward(ctx, conn, nodemesh.TCPKey(port)); err != nil && !errors.Is(err, forward.ErrNoHolder) {
		stage := forward.StageLookup
		var failure *forward.Failure
		if errors.As(err, &failure) {
			stage = failure.Stage
		}
		p.observe(ctx, stage, err)
	}
}

// InjectTCP serves a raw TCP visitor another node forwarded for a tunnel's public port. The decision is this node's
// own, from its authority snapshot (a decision binds the edge node, so one the sender holds is never used), and the
// carrier must be attached here: otherwise it refuses (the forwarder then tries the next holder) before the visitor
// is acknowledged. The existing ForwardPublicTCP path then runs unchanged on a connection whose RemoteAddr is the
// visitor (owner IP lists read it), so metering, idle timeout and revocation are the ordinary ones. On nil it owns conn.
func (p *PublicTCPListeners) InjectTCP(conn net.Conn, port uint16) error {
	p.mu.Lock()
	ctx := p.ctx
	p.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return forward.ErrDraining
	}
	snapshot, cancel := context.WithTimeout(ctx, 2*time.Second)
	decisions, err := p.cfg.Authority.Snapshot(snapshot)
	cancel()
	if err != nil {
		return err
	}
	decision, ok := localPublicTCPDecision(decisions, port)
	if !ok || !p.cfg.Routes.carrierFor(decision) {
		return errNoPublicTCPTunnel
	}
	p.streams.Add(1)
	go func() {
		defer p.streams.Done()
		forwarded := &forwardedTCPConn{Conn: conn, local: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}}
		p.observe(ctx, "public_tcp_stream", p.cfg.Routes.ForwardPublicTCP(ctx, forwarded, decision, p.cfg.Authority.ResolveDecision))
	}()
	return nil
}

// localPublicTCPDecision is the newest valid public tcp decision for port.
func localPublicTCPDecision(decisions []connectorprotocol.IngressDecision, port uint16) (connectorprotocol.IngressDecision, bool) {
	var best connectorprotocol.IngressDecision
	found := false
	for _, d := range decisions {
		if d.Binding.Protocol != "tcp" || d.Binding.Audience != "public" || d.Binding.PublicPort != port || d.Validate(time.Now().UTC()) != nil {
			continue
		}
		if !found || d.AssignmentGeneration > best.AssignmentGeneration {
			best, found = d, true
		}
	}
	return best, found
}

// forwardedTCPConn presents a forwarded visitor as a connection accepted on the tunnel's public port: ForwardPublicTCP
// checks the local port against the decision's.
type forwardedTCPConn struct {
	net.Conn
	local *net.TCPAddr
}

func (c *forwardedTCPConn) LocalAddr() net.Addr { return c.local }

func (c *forwardedTCPConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// carrierFor reports whether the carrier a decision names is attached here: what ForwardPublicTCP looks up.
func (r *DataCarrierRouteRegistry) carrierFor(d connectorprotocol.IngressDecision) bool {
	if r == nil {
		return false
	}
	identity := datacarrier.Identity{AccountID: d.Binding.AccountID, HostID: d.Binding.HostID, TunnelID: d.Binding.TunnelID, ConnectorID: d.ConnectorID, SessionID: d.SessionID, ProcessGeneration: d.ProcessGeneration, Generation: d.ConfigGeneration}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry := r.byKey[carrierRouteIdentityKey(identity)]
	return !r.closed && entry != nil && entry.server != nil
}

// HeldFromDecisions lists the holder keys of the public tunnels this node serves, from its authority snapshot: a raw
// TCP tunnel as tcp:<port> and a TLS-passthrough tunnel as its hostname, each when its decision is valid and its
// carrier is attached (what ForwardPublicTCP would accept).
func (r *DataCarrierRouteRegistry) HeldFromDecisions(decisions []connectorprotocol.IngressDecision) []string {
	var keys []string
	now := time.Now().UTC()
	for _, d := range decisions {
		if d.Binding.Audience != "public" || d.Validate(now) != nil || !r.carrierFor(d) {
			continue
		}
		switch {
		case d.Binding.Protocol == "tcp" && edgeports.PublicTCP(int(d.Binding.PublicPort)):
			keys = append(keys, nodemesh.TCPKey(d.Binding.PublicPort))
		case d.Binding.Protocol == "tls" && d.Binding.PublicPort == 443 && d.Binding.ListenerID == "listener_tls_443":
			if host := strings.ToLower(d.Binding.Hostname); nodemesh.ValidName(host) == nil {
				keys = append(keys, host)
			}
		}
	}
	return keys
}
