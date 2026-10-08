package datacarrier

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
)

var (
	// ErrAccessStreamKind is returned before application bytes are exposed when
	// a host attempts to use the client-initiated access seam for a public or
	// unsupported stream kind.
	ErrAccessStreamKind = errors.New("invalid private access stream kind")
	// ErrAccessStreamIdentity is distinct from a route denial so callers can
	// treat a stale/replaced carrier as unavailable without exposing route
	// existence.
	ErrAccessStreamIdentity      = errors.New("private access stream identity mismatch")
	ErrBrowserTerminalOutputKind = errors.New("invalid browser terminal output stream kind")
)

const (
	// AccessStreamHTTPS carries a bounded private HTTP/CONNECT envelope followed
	// by opaque full-duplex bytes.
	AccessStreamHTTPS = connectorprotocol.PrivateAccessHTTP
	// AccessStreamTCP carries a bounded private raw-TCP envelope followed by
	// opaque full-duplex bytes. It never uses the preview HTTP preface.
	AccessStreamTCP = connectorprotocol.PrivateAccessTCP
	// BrowserTerminalOutputStream carries length-prefixed opaque ciphertext
	// records from the authenticated runtime host to the edge fanout hub.
	BrowserTerminalOutputStream = connectorprotocol.BrowserTerminalOutput
)

// AcceptBrowserTerminalOutputStream accepts one host-initiated terminal output
// stream on an authenticated carrier. The normal carrier authorizer validates
// the exact admitted route and carrier identity before the returned bytes are
// exposed. RequestID is the terminal session ID; the caller owns the stream.
func (s *Server) AcceptBrowserTerminalOutputStream(ctx context.Context) (*Stream, StreamOpen, error) {
	stream, open, err := s.acceptDataStream(ctx, true)
	if err != nil {
		return nil, StreamOpen{}, err
	}
	if open.Kind != BrowserTerminalOutputStream {
		_ = stream.Close()
		return nil, StreamOpen{}, ErrBrowserTerminalOutputKind
	}
	return stream, open, nil
}

// AcceptAccessStream accepts a host-initiated access request on an already
// authenticated carrier. It authenticates only the immutable connector-v1
// carrier identity and stream kind. Private route authorization is deliberately
// performed by edgehttp's PrivateAccessStreamBridge after its bounded machine
// proof envelope has been decoded, so this method cannot accidentally treat a
// carrier connection as route permission.
//
// The returned stream owns one carrier permit and must be closed by the bridge.
// The acceptance context controls only the accept and preface read; it does not
// cancel the returned stream after a successful handoff.
func (s *Server) AcceptAccessStream(ctx context.Context) (*Stream, StreamOpen, error) {
	stream, open, err := s.acceptDataStream(ctx, false)
	if err != nil {
		return nil, StreamOpen{}, err
	}
	if open.Kind != AccessStreamHTTPS && open.Kind != AccessStreamTCP {
		_ = stream.Close()
		return nil, StreamOpen{}, ErrAccessStreamKind
	}
	return stream, open, nil
}
