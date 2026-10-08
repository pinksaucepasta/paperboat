package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/quic-go/quic-go/http3"
	"net/url"
)

// ServeNativePrivateHTTP3 hands one authenticated preview-class native
// session exclusively to HTTP/3 and forwards exactly one CONNECT request.
// Retrying or replaying an HTTP request is intentionally left to the caller.
func ServeNativePrivateHTTP3(ctx context.Context, session *native.Session, authorize func(context.Context, streamauth.Header) (string, error), current NativePrivateTCPCurrent, dial NativePrivateTCPDial, captureStore *inspector.Store) error {
	if ctx == nil || session == nil || authorize == nil || current == nil || dial == nil {
		return ErrNativePrivateBinding
	}
	connection, err := session.HTTP3Connection()
	if err != nil {
		return classifyNativePrivateFailure("stream_open", "native_private_failed", err)
	}
	var handled atomic.Bool
	server := &http3.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !handled.CompareAndSwap(false, true) || request.Method != http.MethodConnect || request.Proto != nativeprivate.HTTP3ConnectProtocol || request.Host != "private-http.paperboat" || request.URL.Path != "/" {
			http.Error(writer, "invalid private HTTP request", http.StatusBadRequest)
			return
		}
		encoded, decodeErr := base64.RawURLEncoding.DecodeString(request.Header.Get("X-Paperboat-Native-Authorization"))
		header, parseErr := streamauth.Parse(encoded, time.Now().UTC())
		var accessSessionID string
		meterAuthorize := func(ctx context.Context, header streamauth.Header) (string, error) {
			var err error
			accessSessionID, err = authorize(ctx, header)
			return accessSessionID, err
		}
		if decodeErr != nil || parseErr != nil || header.Consumer != "private_http" {
			http.Error(writer, "private HTTP access denied", http.StatusForbidden)
			return
		}
		if authorizeErr := session.AuthorizeHTTP3(request.Context(), header, meterAuthorize); authorizeErr != nil {
			if expectedNativePrivateAuthorizationRejection(authorizeErr) {
				http.Error(writer, "private HTTP access denied", http.StatusForbidden)
				return
			}
			reportNativePrivateFailure(request.Context(), "peer_authority", "peer_authority_failed", authorizeErr, true)
			http.Error(writer, "private HTTP authority unavailable", http.StatusServiceUnavailable)
			return
		}
		binding, bindingErr := nativeprivate.Decode([]byte(header.Target), time.Now().UTC())
		if bindingErr != nil || binding.Protocol != "http" {
			http.Error(writer, "private HTTP access denied", http.StatusForbidden)
			return
		}
		validUntil, revoked, currentErr := current(request.Context(), binding)
		if currentErr != nil {
			if expectedNativePrivateBindingFailure(currentErr) || expectedNativePrivateTermination(currentErr) {
				http.Error(writer, "private HTTP access denied", http.StatusForbidden)
				return
			}
			reportNativePrivateFailure(request.Context(), "peer_authority", "peer_authority_failed", currentErr, true)
			http.Error(writer, "private HTTP authority unavailable", http.StatusServiceUnavailable)
			return
		}
		if !validUntil.After(time.Now().UTC()) {
			http.Error(writer, "private HTTP access denied", http.StatusForbidden)
			return
		}
		if binding.ExpiresAt.Before(validUntil) {
			validUntil = binding.ExpiresAt
		}
		// CONNECT preserves origin-owned application TLS. Observe only the
		// authenticated connection binding; never retain ciphertext or pretend
		// that an encrypted stream is a replayable HTTP request.
		if captureStore != nil {
			resourceID := binding.RouteID
			if binding.ResourceKind == "preview" {
				resourceID = binding.ResourceID
			}
			if pending, err := captureStore.TryBegin(resourceID); err == nil {
				defer func() {
					_, _ = captureStore.Finish(pending, inspector.CompletedCapture{
						Method: http.MethodConnect, RawURL: (&url.URL{Scheme: binding.TargetScheme, Host: binding.TargetAddress}).String(),
						ResourceGeneration: binding.ResourceGeneration, RouteGeneration: binding.RouteGeneration, TargetGeneration: binding.TargetGeneration,
						RequestUnsupported: true, ResponseUnsupported: true, ErrorCode: "opaque_native_connection",
					})
				}()
			}
		}
		origin, dialErr := dial(request.Context(), "tcp", binding.TargetAddress)
		if dialErr != nil {
			reportNativePrivateFailure(request.Context(), "target_connect", "native_private_failed", dialErr, false)
			http.Error(writer, "private HTTP origin unavailable", http.StatusBadGateway)
			return
		}
		origin = session.MeterIncoming(origin, header, accessSessionID, true)
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
		type result struct {
			direction string
			err       error
		}
		done := make(chan result, 2)
		go func() {
			_, copyErr := io.Copy(origin, request.Body)
			if closer, ok := origin.(interface{ CloseWrite() error }); ok {
				copyErr = errors.Join(copyErr, closer.CloseWrite())
			}
			done <- result{direction: "request_to_origin", err: copyErr}
		}()
		go func() {
			_, copyErr := io.Copy(flushingNativePrivateWriter{writer}, origin)
			done <- result{direction: "origin_to_response", err: copyErr}
		}()
		var copyErr error
		for range 2 {
			result := <-done
			copyErr = errors.Join(copyErr, result.err)
			if result.direction == "origin_to_response" || result.err != nil && !expectedNativePrivateTermination(result.err) {
				_ = request.Body.Close()
				_ = origin.Close()
			}
		}
		if copyErr != nil {
			reportNativePrivateFailure(request.Context(), "delivery", "native_private_failed", copyErr, true)
		}
	})}
	err = server.ServeQUICConn(connection)
	if expectedNativePrivateTermination(err) {
		return nil
	}
	return classifyNativePrivateFailure("lifecycle", "native_private_failed", err)
}

type flushingNativePrivateWriter struct{ http.ResponseWriter }

func (w flushingNativePrivateWriter) Write(value []byte) (int, error) {
	written, err := w.ResponseWriter.Write(value)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return written, err
}
