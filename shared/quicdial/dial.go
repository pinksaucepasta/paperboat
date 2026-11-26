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
	"time"

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
	byName := func(ctx context.Context) (*quic.Conn, error) {
		remote, err := resolve(ctx, host, port)
		if err != nil {
			return nil, err
		}
		connection, err := dialRemote(ctx, remote, tlsConf, conf, early)
		if err == nil && tlsConf.ServerName == host && len(connection.ConnectionState().TLS.VerifiedChains) > 0 {
			dnscap.Verified(host, connection.RemoteAddr())
		}
		return connection, err
	}
	kept := cached(host)
	if !kept.IsValid() {
		return byName(ctx)
	}
	keptAddress := net.UDPAddrFromAddrPort(netip.AddrPortFrom(kept, uint16(port)))
	connection, err := race(ctx, func(ctx context.Context) (*quic.Conn, error) {
		return dialRemote(ctx, keptAddress, tlsConf, conf, early)
	}, byName)
	if err == nil && connection.RemoteAddr().String() == keptAddress.String() {
		dnscap.Refresh(host, func(ctx context.Context) {
			if found, err := byName(ctx); err == nil {
				_ = found.CloseWithError(0, "")
			}
		})
	}
	return connection, err
}

// cached is the address host last verified at; tests replace it (dnscap never keeps the loopback a test listens on).
var cached = dnscap.Cached

// race dials kept at once (Tailscale net/dnscache: the remembered address, no lookup), and by name too once kept has
// failed or dnscap.KeptDialWait passed; the first connection wins and the other is closed. A slow but working kept
// address keeps the caller's whole budget.
func race(ctx context.Context, kept, byName func(context.Context) (*quic.Conn, error)) (*quic.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		connection *quic.Conn
		err        error
	}
	results := make(chan result, 2)
	start := func(dial func(context.Context) (*quic.Conn, error)) {
		go func() { c, err := dial(ctx); results <- result{c, err} }()
	}
	start(kept)
	wait := time.NewTimer(dnscap.KeptDialWait)
	defer wait.Stop()
	running, named, errs := 1, false, []error(nil)
	for running > 0 {
		select {
		case <-wait.C:
		case r := <-results:
			running--
			if r.err == nil {
				go func(left int) { // the other dial, if any, is closed when it ends
					for ; left > 0; left-- {
						if other := <-results; other.err == nil {
							_ = other.connection.CloseWithError(0, "")
						}
					}
				}(running)
				return r.connection, nil
			}
			errs = append(errs, r.err)
		}
		if !named && ctx.Err() == nil {
			named, running = true, running+1
			start(byName)
		}
	}
	return nil, errors.Join(errs...)
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

// resolve returns the first address of host that a UDP socket can be connected to (a route exists), else the first.
func resolve(ctx context.Context, host string, port int) (*net.UDPAddr, error) {
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
