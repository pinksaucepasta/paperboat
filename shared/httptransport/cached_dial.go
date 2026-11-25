package httptransport

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptrace"

	"github.com/pinksaucepasta/paperboat/shared/dnscap"
)

// cachedDialTLS tries the address the host last verified at before looking its name up, and expires it when the TCP or
// TLS dial fails. An address older than 10 minutes is tried only after 3 s of looking up (Tailscale net/dnscache).
func cachedDialTLS(dial func(context.Context, string, string) (net.Conn, error), config Config) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		kept, fresh := dnscap.Cached(host)
		if kept.IsValid() {
			if !fresh {
				if conn, err := verifiedDial(ctx, dial, config, network, address, host, true); err == nil || ctx.Err() != nil {
					return conn, err
				}
			}
			conn, err := verifiedDial(ctx, dial, config, network, net.JoinHostPort(kept.String(), port), host, true)
			if err == nil {
				return conn, nil
			}
			dnscap.Expire(host, kept)
			if ctx.Err() != nil {
				return nil, err
			}
		}
		return verifiedDial(ctx, dial, config, network, address, host, false)
	}
}

func verifiedDial(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), config Config, network, address, host string, short bool) (net.Conn, error) {
	dialCtx, handshakeWait := ctx, config.TLSHandshakeTimeout
	if short {
		bounded, cancel := context.WithTimeout(ctx, dnscap.LastGoodWait)
		defer cancel()
		dialCtx, handshakeWait = bounded, min(handshakeWait, dnscap.LastGoodWait)
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
	if len(conn.ConnectionState().VerifiedChains) > 0 && tlsConfig.ServerName == host {
		dnscap.Verified(host, raw.RemoteAddr())
	}
	return conn, nil
}
