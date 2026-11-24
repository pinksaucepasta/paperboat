package datacarrier

import (
	"bytes"
	"errors"
)

// ErrCarrierHTTPServer is a failure of the carrier HTTP server itself, not of one connection.
var ErrCarrierHTTPServer = errors.New("data carrier HTTP server reported an internal diagnostic")

var errCarrierConnection = errors.New("data carrier HTTP connection failed")

// Lines net/http and x/net/http2 write for what a remote peer sent (scanners, aborted
// handshakes, protocol errors; Go 1.27 net/http/server.go, x/net v0.60 http2/server.go).
// Prior art: Caddy and Tailscale drop TLS handshake errors from the server error log.
var clientCausedDiagnostics = [][]byte{
	[]byte("http: TLS handshake error"),
	[]byte("http2: server: error reading preface"),
	[]byte("http2: server connection error from"),
	[]byte("http2: server closing client connection"),
	[]byte("timeout waiting for SETTINGS frames"),
	[]byte("timeout waiting for PING response"),
}

// ClientCausedDiagnostic reports whether an HTTP server log line is about one peer's connection.
func ClientCausedDiagnostic(line []byte) bool {
	for _, prefix := range clientCausedDiagnostics {
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
