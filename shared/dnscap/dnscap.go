// Package dnscap is the resolver every Paperboat dial looks names up with.
package dnscap

import (
	"context"
	"net"
	"time"
)

// dnsServerWait caps how long one DNS server is waited for. The resolver's own 5 s per dead server, and again for the
// second address family with resolv.conf's single-request, made a lookup take 10 s behind a dead first nameserver:
// longer than the check that decides whether Paperboat is reachable. tailscaled races its upstreams instead
// (net/dns/resolver/forwarder.go); this keeps the system's order.
// shortcut: a server that needs over 3 s for every answer is skipped, race the servers if that is ever seen.
const dnsServerWait = 3 * time.Second

type cappedDNSConn struct{ *net.UDPConn }

func (c cappedDNSConn) SetDeadline(t time.Time) error {
	if limit := time.Now().Add(dnsServerWait); t.IsZero() || t.After(limit) {
		t = limit
	}
	return c.UDPConn.SetDeadline(t)
}

// New is a resolver of its own (see shared/httptransport on joined lookups) with the cap. Only Go's own resolver dials
// (Linux); macOS and Windows ask the system.
func New() *net.Resolver {
	return &net.Resolver{Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if udp, ok := conn.(*net.UDPConn); ok {
			return cappedDNSConn{udp}, err
		}
		return conn, err
	}}
}
