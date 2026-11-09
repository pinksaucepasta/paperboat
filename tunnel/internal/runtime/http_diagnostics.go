package runtime

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

var errHTTPServerDiagnostic = errors.New("HTTP server reported an internal diagnostic")

// net/http supplies already formatted messages containing remote addresses and
// arbitrary panic/error text. Keep a finite signal rather than parsing them.
type httpDiagnosticWriter struct {
	ctx      context.Context
	reporter *reporting.Reporter
}

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

func (w httpDiagnosticWriter) Write(data []byte) (int, error) {
	for _, prefix := range clientCausedDiagnostics {
		if bytes.HasPrefix(data, prefix) {
			return len(data), nil
		}
	}
	w.reporter.ObserveFailure(w.ctx, "http_server", errHTTPServerDiagnostic)
	return len(data), nil
}
func httpDiagnosticLogger(ctx context.Context, reporter *reporting.Reporter) *log.Logger {
	return log.New(httpDiagnosticWriter{ctx: ctx, reporter: reporter}, "", 0)
}
func httpDiagnosticHandler(ctx context.Context, reporter *reporting.Reporter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				if value != http.ErrAbortHandler {
					reporter.CaptureFailure(reporting.WithSupportReference(request.Context(), reporting.SupportReference(ctx)), "process_panic", reporting.PanicFailure{})
				}
				// Both net/http and HTTP/3 deliberately suppress ErrAbortHandler logging
				// and abort the stream. No panic payload or partial successful response.
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(writer, request)
	})
}
