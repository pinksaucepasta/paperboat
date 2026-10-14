package filetransfer

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

// NewNativeRoundTripper carries the existing HTTP file-transfer protocol over
// an authenticated Paperboat native stream. QUIC already owns TLS, so both
// HTTP Dial hooks return that encrypted stream without a second TLS layer.
func NewNativeRoundTripper(open func(context.Context) (net.Conn, error)) (http.RoundTripper, error) {
	if open == nil {
		return nil, errors.New("native file transfer stream opener is required")
	}
	transport := &http.Transport{ForceAttemptHTTP2: false, DisableCompression: true, MaxConnsPerHost: 4, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return open(ctx) }
	transport.DialTLSContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return open(ctx) }
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	return transport, nil
}
