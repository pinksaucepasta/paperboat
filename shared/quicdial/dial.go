// Package quicdial dials QUIC servers by name the way a TCP dial does. quic.DialAddr binds an IPv4 socket and
// resolves with net.ResolveUDPAddr, which returns the first IPv4 address whenever a name has one, so a host without
// an IPv4 route (IPv6-only behind NAT64+DNS64, where the name's A record is unreachable and the synthesized AAAA is
// the only way out) can never complete a handshake. Prior art: Tailscale's DERP client dials host names through the
// system resolver (derphttp/derphttp_client.go dialNode) and Apple requires IPv6-only-safe name dials.
package quicdial

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strconv"

	"github.com/pinksaucepasta/paperboat/shared/dnscap"
	"github.com/quic-go/quic-go"
)

// Dial resolves address (host:port, the host a name or an IP literal), takes the first address that has a route from
// this host (the resolver already orders them by RFC 6724) and opens a socket of that family. The TLS server name is
// the host unless tlsConf sets one. The socket closes with the connection.
func Dial(ctx context.Context, address string, tlsConf *tls.Config, conf *quic.Config, early bool) (*quic.Conn, error) {
	if tlsConf == nil {
		return nil, errors.New("quic: tls.Config not set")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("quic: invalid port in " + address)
	}
	tlsConf = tlsConf.Clone()
	if tlsConf.ServerName == "" {
		tlsConf.ServerName = host
	}
	remote, kept, err := resolve(ctx, host, port)
	if err != nil {
		return nil, err
	}
	connection, err := dialRemote(ctx, remote, tlsConf, conf, early)
	if err != nil && kept.IsValid() {
		dnscap.Expire(host, kept)
		if again, lookupErr := route(ctx, host, port); lookupErr == nil && !again.IP.Equal(remote.IP) {
			connection, err = dialRemote(ctx, again, tlsConf, conf, early)
		}
	}
	if err == nil && tlsConf.ServerName == host && len(connection.ConnectionState().TLS.VerifiedChains) > 0 {
		dnscap.Verified(host, connection.RemoteAddr())
	}
	return connection, err
}

func dialRemote(ctx context.Context, remote *net.UDPAddr, tlsConf *tls.Config, conf *quic.Config, early bool) (*quic.Conn, error) {
	network, local := "udp6", &net.UDPAddr{IP: net.IPv6unspecified}
	if remote.IP.To4() != nil {
		network, local = "udp4", &net.UDPAddr{IP: net.IPv4zero}
	}
	socket, err := net.ListenUDP(network, local)
	if err != nil {
		return nil, err
	}
	transport := &quic.Transport{Conn: socket}
	var connection *quic.Conn
	if early {
		connection, err = transport.DialEarly(ctx, remote, tlsConf, conf)
	} else {
		connection, err = transport.Dial(ctx, remote, tlsConf, conf)
	}
	if err != nil {
		_ = transport.Close()
		_ = socket.Close()
		return nil, err
	}
	// AfterFunc, not a goroutine that waits: an idle connection must not cost a goroutine more than quic.DialAddr's.
	context.AfterFunc(connection.Context(), func() {
		_ = transport.Close()
		_ = socket.Close()
	})
	return connection, nil
}

// lookup is the system resolver, a fresh one per call: the shared one joins a lookup of the same name already in
// flight, so a relay redial after an outage waited for a query sent while the network was down. Tests replace it.
var lookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return dnscap.New().LookupIPAddr(ctx, host)
}

// resolve returns the address host last verified at (kept is then set), or its route: at once while fresh, else only
// when the lookup fails.
func resolve(ctx context.Context, host string, port int) (remote *net.UDPAddr, kept netip.Addr, err error) {
	kept, fresh := dnscap.Cached(host)
	if !fresh {
		lookupCtx, cancel := ctx, func() {}
		if kept.IsValid() {
			lookupCtx, cancel = context.WithTimeout(ctx, dnscap.LastGoodWait)
		}
		defer cancel()
		if remote, err = route(lookupCtx, host, port); err == nil || !kept.IsValid() {
			return remote, netip.Addr{}, err
		}
	}
	return net.UDPAddrFromAddrPort(netip.AddrPortFrom(kept, uint16(port))), kept, nil
}

// route returns the first address of host that a UDP socket can be connected to (a route exists), else the first.
func route(ctx context.Context, host string, port int) (*net.UDPAddr, error) {
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{Err: "no addresses", Name: host}
	}
	first := &net.UDPAddr{IP: ips[0].IP, Port: port, Zone: ips[0].Zone}
	for _, ip := range ips {
		candidate := &net.UDPAddr{IP: ip.IP, Port: port, Zone: ip.Zone}
		if probe, err := net.DialUDP("udp", nil, candidate); err == nil {
			_ = probe.Close()
			return candidate, nil
		}
	}
	return first, nil
}
