package tunnelmanager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

const maximumInspectorStreamRequest = (64 << 10) + maximumOriginRequestHeaderBytes

var ErrInspectorStreamInvalid = errors.New("invalid inspector carrier stream")

type AuthenticatedInspectorHTTP interface {
	ServeAuthenticatedHTTP(http.ResponseWriter, *http.Request)
}

// ServeInspectorStream serves exactly one bounded inspector HTTP operation on
// an authenticated owner carrier. The caller has already matched the carrier
// identity and must dispatch only InspectorHTTP streams. This adapter never
// opens an application origin or retains request/response payloads.
func ServeInspectorStream(ctx context.Context, stream io.ReadWriteCloser, open connectorprotocol.StreamOpen, service AuthenticatedInspectorHTTP) error {
	if ctx == nil || stream == nil || service == nil || open.Validate() != nil || open.Kind != connectorprotocol.InspectorHTTP {
		return ErrInspectorStreamInvalid
	}
	request, err := http.ReadRequest(bufio.NewReader(io.LimitReader(stream, maximumInspectorStreamRequest)))
	if err != nil {
		return fmt.Errorf("%w: request", ErrInspectorStreamInvalid)
	}
	defer request.Body.Close()
	if !validInspectorStreamOperation(request.Method, request.URL.Path) || request.Host != "paperboatd.local" {
		return fmt.Errorf("%w: operation", ErrInspectorStreamInvalid)
	}
	request = request.WithContext(ctx)
	writer := &inspectorStreamResponseWriter{stream: stream, header: make(http.Header)}
	service.ServeAuthenticatedHTTP(writer, request)
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.err
}

type inspectorStreamResponseWriter struct {
	stream      io.Writer
	header      http.Header
	wroteHeader bool
	err         error
}

func (w *inspectorStreamResponseWriter) Header() http.Header { return w.header }

func (w *inspectorStreamResponseWriter) WriteHeader(status int) {
	if w.wroteHeader || w.err != nil {
		return
	}
	w.wroteHeader = true
	contentType := w.header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.err = w.write([]byte(fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: %s\r\nCache-Control: no-store\r\nPragma: no-cache\r\nConnection: close\r\n\r\n", status, http.StatusText(status), contentType)))
}

func (w *inspectorStreamResponseWriter) Write(payload []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.err != nil {
		return 0, w.err
	}
	if err := w.write(payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (w *inspectorStreamResponseWriter) write(payload []byte) error {
	if setter, ok := w.stream.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := setter.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			w.err = err
			return err
		}
	}
	for len(payload) > 0 {
		written, err := w.stream.Write(payload)
		if err != nil {
			w.err = err
			return err
		}
		if written == 0 {
			w.err = io.ErrShortWrite
			return w.err
		}
		payload = payload[written:]
	}
	return nil
}

func validInspectorStreamOperation(method, path string) bool {
	switch {
	case method == http.MethodGet && path == "/v1/inspector/records":
		return true
	case method == http.MethodGet && strings.HasPrefix(path, "/v1/inspector/records/") && !strings.Contains(strings.TrimPrefix(path, "/v1/inspector/records/"), "/"):
		return true
	case method == http.MethodGet && path == "/v1/inspector/audit":
		return true
	case method == http.MethodDelete && path == "/v1/inspector/records":
		return true
	case method == http.MethodPut && path == "/v1/inspector/policy":
		return true
	case method == http.MethodPost && path == "/v1/inspector/replay":
		return true
	default:
		return false
	}
}
