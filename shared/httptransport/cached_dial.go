package httptransport

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptrace"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/dnscap"
)

// cachedDialTLS dials the address the host last verified at and looks its name up only when there is none or it fails.
func cachedDialTLS(dial func(context.Context, string, string) (net.Conn, error), config Config) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if kept := dnscap.Cached(host); kept.IsValid() {
			conn, err := verifiedDial(ctx, dial, config, network, net.JoinHostPort(kept.String(), port), host, dnscap.KeptDialWait)
			if err == nil {
				dnscap.Refresh(host, func(ctx context.Context) {
					if conn, err := verifiedDial(ctx, dial, config, network, address, host, 0); err == nil {
						_ = conn.Close()
					}
				})
			}
			if err == nil || ctx.Err() != nil {
				return conn, err
			}
		}
		return verifiedDial(ctx, dial, config, network, address, host, 0)
	}
}

// verifiedDial dials address, limited to wait (0: none) each for TCP and TLS, and keeps where a looked-up name verified.
func verifiedDial(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), config Config, network, address, host string, wait time.Duration) (net.Conn, error) {
	dialCtx, handshakeWait := ctx, config.TLSHandshakeTimeout
	if wait > 0 {
		bounded, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		dialCtx, handshakeWait = bounded, min(handshakeWait, wait)
	}
	raw, err := dial(dialCtx, network, address)
	if err != nil {
		return nil, err
	}
	tlsConfig := config.TLSConfig.Clone()
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = host
	}
	if config.ForceAttemptHTTP2 && len(tlsConfig.NextProtos) == 0 {
		tlsConfig.NextProtos = []string{"h2", "http/1.1"}
	}
	conn := tls.Client(raw, tlsConfig)
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, handshakeWait)
	defer cancelHandshake()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		_ = raw.Close()
		if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.TLSHandshakeDone != nil {
			trace.TLSHandshakeDone(tls.ConnectionState{}, err) // net/http calls it itself after a handshake that worked
		}
		return nil, err
	}
	if wait == 0 && len(conn.ConnectionState().VerifiedChains) > 0 && tlsConfig.ServerName == host {
		dnscap.Verified(host, raw.RemoteAddr())
	}
	return conn, nil
}
