// Package quicgate bounds the unauthenticated QUIC handshakes one source address may have unfinished at once, and
// decides when an address must prove itself with a Retry (RFC 9000 8.1.2). Prior art: quic-go's
// Transport.VerifySourceAddress guidance ("under load" retry); ~/ref/cloudflared/quic runs quic-go with its defaults.
package quicgate

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/quic-go/quic-go"

	"github.com/pinksaucepasta/paperboat/shared/addrlimit"
)

// ErrPending refuses a connection from an address that already has its bound of unfinished handshakes.
var ErrPending = errors.New("QUIC handshake refused: this address has too many unfinished handshakes")

type settleKey struct{}

// NewTransport gates a QUIC socket: at most perAddress connections of one address (IPv4 /32, IPv6 /64) may be unfinished;
// the rest get CONNECTION_REFUSED. retryAlways makes every address prove itself with a Retry first (right for long-lived
// machine carriers, which connect a few times each); otherwise only an address that already has an unfinished
// handshake must (right for visitors: a fresh address pays no extra round trip, a flood from one address does, and a
// spoofed address cannot answer a Retry, so it cannot spend another address's budget beyond its first handshake).
func NewTransport(conn net.PacketConn, perAddress int, retryAlways bool) *quic.Transport {
	pending := addrlimit.NewInFlight(perAddress)
	return &quic.Transport{
		Conn: conn,
		VerifySourceAddress: func(addr net.Addr) bool {
			return retryAlways || pending.Active(host(addr)) > 0
		},
		ConnContext: func(ctx context.Context, client *quic.ClientInfo) (context.Context, error) {
			release, ok := pending.Admit(host(client.RemoteAddr), false)
			if !ok {
				return nil, ErrPending
			}
			var once sync.Once
			settle := func() { once.Do(release) }
			context.AfterFunc(ctx, settle) // a handshake that fails or a connection that closes frees its slot
			return context.WithValue(ctx, settleKey{}, settle), nil
		},
	}
}

// Settle frees the connection's pending-handshake slot once its handshake (and admission) are done.
func Settle(ctx context.Context) {
	if settle, ok := ctx.Value(settleKey{}).(func()); ok {
		settle()
	}
}

func host(addr net.Addr) string {
	h, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return h
}
