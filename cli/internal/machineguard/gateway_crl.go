//go:build linux || darwin || windows

package machineguard

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// The CRL distribution point of every local leaf is http://127.100.0.1/.paperboat/crl/<hash>.crl. On Windows the
// fetch is made by lsass.exe (SYSTEM) for Schannel clients such as curl.exe, which the gateway cannot attribute to a
// user's daemon, so it would be refused and the TLS handshake would fail with CRYPT_E_REVOCATION_OFFLINE. The CRL is
// public data: the guard that owns the CA answers it itself, before attribution, for exactly that request.
const (
	gatewayCRLRequestLimit = 512
	gatewayCRLReadWindow   = 2 * time.Second
)

// serveGatewayCRL reads the request line of a plain-HTTP gateway connection. A GET or HEAD of a CRL path that lookup
// knows is answered, the connection closed and (nil, true) returned; anything else is returned as a connection that replays the bytes read,
// so attribution and delivery see the request untouched.
func serveGatewayCRL(accepted net.Conn, lookup func(path string) []byte) (net.Conn, bool) {
	_ = accepted.SetReadDeadline(time.Now().Add(gatewayCRLReadWindow))
	reader := bufio.NewReaderSize(accepted, gatewayCRLRequestLimit)
	line, _ := reader.ReadSlice('\n') // a long or unterminated line just does not match
	replay := replayConn{Conn: accepted, reader: io.MultiReader(bytes.NewReader(bytes.Clone(line)), reader)}
	_ = accepted.SetReadDeadline(time.Time{})
	fields := strings.Fields(string(line))
	if len(fields) != 3 || (fields[0] != "GET" && fields[0] != "HEAD") || !strings.HasPrefix(fields[2], "HTTP/1.") ||
		!strings.HasPrefix(fields[1], splitdns.CRLPathPrefix) || strings.ContainsAny(fields[1], "?#") {
		return replay, false
	}
	der := lookup(fields[1])
	if der == nil {
		return replay, false
	}
	_ = accepted.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(accepted, "HTTP/1.1 200 OK\r\nContent-Type: application/pkix-crl\r\nCache-Control: max-age=3600\r\nX-Content-Type-Options: nosniff\r\nConnection: close\r\nContent-Length: "+strconv.Itoa(len(der))+"\r\n\r\n")
	if fields[0] == "GET" {
		_, _ = accepted.Write(der)
	}
	accepted.Close()
	return nil, true
}

// replayConn reads the bytes already taken from the connection, then the rest.
type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c replayConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// CloseWrite lets the accept broker half-close a replayed connection like the TCP one it wraps.
func (c replayConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}
