package datacarrier

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/quic-go/quic-go"

	"github.com/pinksaucepasta/paperboat/shared/addrlimit"
)

var errCarrierQUICPending = errors.New("data carrier QUIC handshake refused: this address has too many unfinished handshakes")

type quicSettleKey struct{}

// newQUICTransport gates a carrier QUIC socket the way handshakeListener gates TCP: every address first proves
// itself with a Retry (a spoofed address answers nothing, so it creates no connection state and cannot spend
// another address's budget), then at most pendingHandshakesPerAddress of its connections may be unfinished;
// the rest get CONNECTION_REFUSED. Retry costs every carrier connect one round trip, and carriers are long-lived
// (a few connects per machine), so it is applied to all addresses rather than only to ones with pending
// handshakes: that needs no load heuristic and leaves no unvalidated path. Prior art: quic-go's own
// Transport.VerifySourceAddress guidance (RFC 9000 8.1.2); ~/ref/cloudflared/quic runs quic-go with its defaults.
func newQUICTransport(conn net.PacketConn) *quic.Transport {
	pending := addrlimit.NewInFlight(pendingHandshakesPerAddress)
	return &quic.Transport{
		Conn:                conn,
		VerifySourceAddress: func(net.Addr) bool { return true },
		ConnContext: func(ctx context.Context, client *quic.ClientInfo) (context.Context, error) {
			host, _, err := net.SplitHostPort(client.RemoteAddr.String())
			if err != nil {
				host = client.RemoteAddr.String()
			}
			release, ok := pending.Admit(host, false)
			if !ok {
				return nil, errCarrierQUICPending
			}
			var once sync.Once
			settle := func() { once.Do(release) }
			context.AfterFunc(ctx, settle) // a handshake that fails or a connection that closes frees its slot
			return context.WithValue(ctx, quicSettleKey{}, settle), nil
		},
	}
}

// settleQUICHandshake frees the connection's pending-handshake slot once its handshake and admission are done.
func settleQUICHandshake(ctx context.Context) {
	if settle, ok := ctx.Value(quicSettleKey{}).(func()); ok {
		settle()
	}
}
