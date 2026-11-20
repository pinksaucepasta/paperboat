package datacarrier

import (
	"context"
	"net"

	"github.com/quic-go/quic-go"

	"github.com/pinksaucepasta/paperboat/edge/internal/quicgate"
)

// newQUICTransport gates a carrier QUIC socket the way handshakeListener gates TCP. Carriers are long-lived (a few
// connects per machine), so every address proves itself with a Retry first: one extra round trip per connect and no
// unvalidated path.
func newQUICTransport(conn net.PacketConn) *quic.Transport {
	return quicgate.NewTransport(conn, pendingHandshakesPerAddress, true)
}

// settleQUICHandshake frees the connection's pending-handshake slot once its handshake and admission are done.
func settleQUICHandshake(ctx context.Context) { quicgate.Settle(ctx) }
