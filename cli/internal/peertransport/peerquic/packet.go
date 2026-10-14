package peerquic

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	"github.com/quic-go/quic-go"
)

// PacketSocket is an owned, connected datagram lease. Close must be idempotent.
// The caller transfers ownership on entry, including on failed setup.
type PacketSocket interface {
	net.PacketConn
	RemoteAddr() net.Addr
	MaxPayload() uint16
}

func packetConfig(socket PacketSocket, config SessionConfig) (*quic.Config, error) {
	if socket == nil {
		return nil, errors.New("peer QUIC requires a packet socket")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	if socket.MaxPayload() < 1200 || config.InitialPacketSize > socket.MaxPayload() {
		return nil, errors.New("peer QUIC packet size exceeds virtual UDP payload limit")
	}
	q := quicConfig(config)
	// The virtual MTU is fixed. With discovery disabled quic-go uses the initial
	// size throughout the connection; physical path probing belongs to Tailcat.
	q.DisablePathMTUDiscovery = true
	return q, nil
}

func DialPacket(ctx context.Context, socket PacketSocket, tlsConfig *tls.Config, config SessionConfig) (session *Session, err error) {
	defer func() {
		if err != nil && socket != nil {
			_ = socket.Close()
		}
	}()
	q, err := packetConfig(socket, config)
	if err != nil {
		return nil, err
	}
	if err = validateClientTLS(tlsConfig); err != nil {
		return nil, err
	}
	session, err = dialPacket(ctx, socket, socket.RemoteAddr(), tlsConfig, q)
	if err == nil && ctx.Err() != nil {
		_ = session.Close()
		return nil, ctx.Err()
	}
	return session, err
}

func ListenPacket(socket PacketSocket, tlsConfig *tls.Config, config SessionConfig) (listener *Listener, err error) {
	defer func() {
		if err != nil && socket != nil {
			_ = socket.Close()
		}
	}()
	q, err := packetConfig(socket, config)
	if err != nil {
		return nil, err
	}
	if err = validateServerTLS(tlsConfig); err != nil {
		return nil, err
	}
	return listenPacket(socket, tlsConfig, q)
}
