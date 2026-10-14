package edgehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// The inspection budget includes TLS record headers and read-ahead. Concurrent
// inspections share the listener's existing connection admission bound.
const maximumClientHelloBytes = 64 << 10
const clientHelloTimeout = 5 * time.Second

var errClientHello = errors.New("TLS ingress requires a bounded ClientHello with an exact SNI hostname")
var errHelloInspected = errors.New("ClientHello inspection complete")

type clientHelloFailure struct{ cause error }

func (e *clientHelloFailure) Error() string        { return "TLS ingress ClientHello could not be validated" }
func (e *clientHelloFailure) Unwrap() error        { return e.cause }
func (e *clientHelloFailure) Is(target error) bool { return target == errClientHello }

// helloCapture lets the maintained Go TLS parser inspect fragmented records,
// but never sends its alerts or handshake bytes to the peer. No TLS session is
// established here. Every byte read is replayed to the authorized origin.
type helloCapture struct {
	net.Conn
	inspected bytes.Buffer
}

func (c *helloCapture) Read(p []byte) (int, error) {
	remaining := maximumClientHelloBytes - c.inspected.Len()
	if remaining == 0 {
		return 0, errClientHello
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := c.Conn.Read(p)
	c.inspected.Write(p[:n])
	return n, err
}

func (c *helloCapture) Write([]byte) (int, error) { return 0, errHelloInspected }

type replayTLSConn struct {
	net.Conn
	reader         io.Reader
	hostname       string
	sharedListener bool
}

func (c *replayTLSConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *replayTLSConn) CloseWrite() error {
	return c.Conn.(interface{ CloseWrite() error }).CloseWrite()
}

func inspectTLSClientHello(ctx context.Context, connection net.Conn) (net.Conn, string, error) {
	return inspectTLSInfrastructureClientHello(ctx, connection, "")
}
func inspectTLSInfrastructureClientHello(ctx context.Context, connection net.Conn, infrastructureHost string) (net.Conn, string, error) {
	if ctx == nil || connection == nil {
		return nil, "", errClientHello
	}
	if _, ok := connection.(interface{ CloseWrite() error }); !ok {
		return nil, "", errClientHello
	}
	inspectCtx, cancel := context.WithTimeout(ctx, clientHelloTimeout)
	defer cancel()
	deadline, _ := inspectCtx.Deadline()
	if err := connection.SetReadDeadline(deadline); err != nil {
		return nil, "", err
	}
	capture := &helloCapture{Conn: connection}
	hostname := ""
	parser := tls.Server(capture, &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		hostname = strings.ToLower(hello.ServerName)
		return nil, errHelloInspected
	}})
	err := parser.HandshakeContext(inspectCtx)
	if hostname == "" && net.ParseIP(infrastructureHost) != nil {
		hostname = infrastructureHost
	}
	infrastructureIP := hostname == infrastructureHost && net.ParseIP(infrastructureHost) != nil
	if !errors.Is(err, errHelloInspected) || (!validSNIHostname(hostname) && !infrastructureIP) || inspectCtx.Err() != nil {
		cause := err
		if errors.Is(cause, errHelloInspected) {
			cause = nil
		}
		cause = errors.Join(cause, inspectCtx.Err())
		if cause == nil {
			return nil, "", errClientHello
		}
		return nil, "", &clientHelloFailure{cause: cause}
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return nil, "", err
	}
	return &replayTLSConn{Conn: connection, hostname: hostname, reader: io.MultiReader(bytes.NewReader(capture.inspected.Bytes()), connection)}, hostname, nil
}

func validSNIHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
				return false
			}
		}
	}
	return true
}
