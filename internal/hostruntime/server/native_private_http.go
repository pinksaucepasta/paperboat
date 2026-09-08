package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/quic-go/quic-go/http3"
)

// ServeNativePrivateHTTP3 hands one authenticated preview-class native
// session exclusively to HTTP/3 and forwards exactly one CONNECT request.
// Retrying or replaying an HTTP request is intentionally left to the caller.
func ServeNativePrivateHTTP3(ctx context.Context, session *native.Session, authorize func(context.Context, streamauth.Header) (string, error), current NativePrivateTCPCurrent, dial NativePrivateTCPDial) error {
	if ctx == nil || session == nil || authorize == nil || current == nil || dial == nil {
		return ErrNativePrivateBinding
	}
	connection, err := session.HTTP3Connection()
	if err != nil {
		return err
	}
	var handled atomic.Bool
	server := &http3.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !handled.CompareAndSwap(false, true) || request.Method != http.MethodConnect || request.Proto != nativeprivate.HTTP3ConnectProtocol || request.Host != "private-http.paperboat" || request.URL.Path != "/" {
			http.Error(writer, "invalid private HTTP request", http.StatusBadRequest)
			return
		}
		encoded, decodeErr := base64.RawURLEncoding.DecodeString(request.Header.Get("X-Paperboat-Native-Authorization"))
		header, parseErr := streamauth.Parse(encoded, time.Now().UTC())
		if decodeErr != nil || parseErr != nil || header.Consumer != "private_http" || session.AuthorizeHTTP3(request.Context(), header, authorize) != nil {
			http.Error(writer, "private HTTP access denied", http.StatusForbidden)
			return
		}
		binding, bindingErr := nativeprivate.Decode([]byte(header.Target), time.Now().UTC())
		if bindingErr != nil || binding.Protocol != "http" {
			http.Error(writer, "private HTTP access denied", http.StatusForbidden)
			return
		}
		validUntil, revoked, currentErr := current(request.Context(), binding)
		if currentErr != nil || !validUntil.After(time.Now().UTC()) {
			http.Error(writer, "private HTTP access denied", http.StatusForbidden)
			return
		}
		if binding.ExpiresAt.Before(validUntil) {
			validUntil = binding.ExpiresAt
		}
		origin, dialErr := dial(request.Context(), "tcp", binding.TargetAddress)
		if dialErr != nil {
			http.Error(writer, "private HTTP origin unavailable", http.StatusBadGateway)
			return
		}
		defer origin.Close()
		lifetime, cancel := context.WithDeadline(request.Context(), validUntil)
		defer cancel()
		go func() {
			select {
			case <-lifetime.Done():
				_ = origin.Close()
			case <-revoked:
				_ = origin.Close()
			case <-request.Context().Done():
			}
		}()
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		type result struct{ err error }
		done := make(chan result, 2)
		go func() {
			_, copyErr := io.Copy(origin, request.Body)
			if closer, ok := origin.(interface{ CloseWrite() error }); ok {
				copyErr = errors.Join(copyErr, closer.CloseWrite())
			}
			done <- result{copyErr}
		}()
		go func() { _, copyErr := io.Copy(flushingNativePrivateWriter{writer}, origin); done <- result{copyErr} }()
		<-done
		<-done
	})}
	err = server.ServeQUICConn(connection)
	if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

type flushingNativePrivateWriter struct{ http.ResponseWriter }

func (w flushingNativePrivateWriter) Write(value []byte) (int, error) {
	written, err := w.ResponseWriter.Write(value)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return written, err
}
