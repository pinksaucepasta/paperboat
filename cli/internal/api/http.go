package api

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/httptransport"
)

const (
	defaultRequestTimeout      = 30 * time.Second
	defaultTLSHandshakeTimeout = 5 * time.Second
	tlsHandshakeAttempts       = 3
)

func defaultHTTPClient() *http.Client {
	config := httptransport.DevelopmentConfig()
	config.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	transport, err := httptransport.New(config)
	if err != nil {
		panic(err)
	}
	return &http.Client{
		Timeout: defaultRequestTimeout,
		Transport: &tlsHandshakeRetryTransport{
			base:     transport,
			attempts: tlsHandshakeAttempts,
		},
	}
}

// tlsHandshakeRetryTransport retries only failures that happen before an HTTP
// request is sent. Ambiguous transport failures are returned immediately so
// credential-rotating and other mutation requests are never replayed.
type tlsHandshakeRetryTransport struct {
	base     http.RoundTripper
	attempts int
}

func (t *tlsHandshakeRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempts := t.attempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		request, err := requestForAttempt(req, attempt)
		if err != nil {
			return nil, err
		}
		var tlsFailed, wroteHeaders atomic.Bool
		trace := &httptrace.ClientTrace{
			TLSHandshakeDone: func(_ tls.ConnectionState, err error) { tlsFailed.Store(err != nil) },
			WroteHeaders:     func() { wroteHeaders.Store(true) },
		}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
		resp, err := t.base.RoundTrip(request)
		if err == nil || !retryableTransportError(request, err, tlsFailed.Load(), wroteHeaders.Load()) || attempt == attempts-1 {
			return resp, err
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	panic("unreachable")
}

func requestForAttempt(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 || req.Body == nil {
		return req, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("cannot retry TLS handshake: request body is not replayable")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

func retryableTransportError(req *http.Request, err error, tlsFailed, wroteHeaders bool) bool {
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		return false
	}
	// The standard transport's TLSHandshakeDone callback proves this timeout
	// preceded HTTP headers. Error text cannot establish mutation replay safety.
	if tlsFailed && !wroteHeaders {
		return true
	}
	// Read-only requests may safely retry timeouts at any transport phase.
	return req.Method == http.MethodGet || req.Method == http.MethodHead
}
