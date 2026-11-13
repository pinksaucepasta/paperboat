package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

type NativeDispatchClientConfig struct {
	Address, Token string
	DialTimeout    time.Duration
}
type NativeDispatchClient struct{ config NativeDispatchClientConfig }

func NewNativeDispatchClient(config NativeDispatchClientConfig) (*NativeDispatchClient, error) {
	host, _, err := net.SplitHostPort(config.Address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || len(config.Token) < 32 || len(config.Token) > 128 || config.DialTimeout <= 0 || config.DialTimeout > 30*time.Second {
		return nil, ErrStreamAuthorization
	}
	return &NativeDispatchClient{config: config}, nil
}

type nativeDispatchConn struct {
	net.Conn
	reader     *bufio.Reader
	stop       func() bool
	readBytes  atomic.Uint64
	writeBytes atomic.Uint64
}

func (c *nativeDispatchConn) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if n > 0 && diagnostics.TerminalHopsEnabled() {
		end := c.readBytes.Add(uint64(n))
		diagnostics.TerminalHop("carrier_host_read", c.traceIdentity(), end-uint64(n), end, time.Now())
	}
	return n, err
}

// Both ends of the protected loopback connection use the same opaque diagnostic label.
func (c *nativeDispatchConn) traceIdentity() string {
	local, remote := c.LocalAddr().String(), c.RemoteAddr().String()
	if local > remote {
		local, remote = remote, local
	}
	return local + "_" + remote
}

func (c *nativeDispatchConn) Write(p []byte) (int, error) {
	if !diagnostics.TerminalHopsEnabled() {
		return c.Conn.Write(p)
	}
	start := c.writeBytes.Load()
	diagnostics.TerminalHop("host_carrier_write_start", c.traceIdentity(), start, start+uint64(len(p)), time.Now())
	n, err := c.Conn.Write(p)
	end := c.writeBytes.Add(uint64(n))
	diagnostics.TerminalHop("host_carrier_write_end", c.traceIdentity(), end-uint64(n), end, time.Now())
	return n, err
}

func (c *nativeDispatchConn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return c.Conn.Close()
}
func (c *nativeDispatchConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return c.Close()
}
func nativeDispatchHijack(ctx context.Context, writer http.ResponseWriter) (net.Conn, error) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return nil, ErrNativeDispatchUnavailable
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	if lifetime, ok := ctx.Value(nativeDispatchLifetimeKey{}).(nativeDispatchRequestLifetime); ok {
		lifetime.stopRequest()
	}
	if err := ctx.Err(); err != nil {
		connection.Close()
		return nil, err
	}
	if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = buffer.Flush()
	}
	if err != nil {
		connection.Close()
		return nil, err
	}
	return &nativeDispatchConn{Conn: connection, reader: buffer.Reader}, nil
}
func (c *NativeDispatchClient) call(
	ctx context.Context, operation string, header *streamauth.Header,
	binding *nativeprivate.Binding, resume *NativeDispatchResumePeer, offline *NativeDispatchOfflinePeer, stream bool,
) (net.Conn, nativeDispatchResponse, error) {
	var result nativeDispatchResponse
	if c == nil || ctx == nil {
		return nil, result, ErrStreamAuthorization
	}
	deadline := time.Now().Add(c.config.DialTimeout)
	if operation == "stream" && header != nil {
		deadline = time.Now().Add(serverclock.Until(time.Unix(header.DeadlineUnix, 0))) // the caller's deadline is on the server's clock
	}
	var lastErr error
	for {
		if lastErr != nil && !time.Now().Before(deadline) {
			return nil, result, errors.Join(lastErr, context.DeadlineExceeded)
		}
		connection, answer, err := c.callOnce(ctx, operation, header, binding, resume, offline, stream, deadline)
		result = answer
		if err == nil || operation != "stream" || !nativeDispatchTemporary(err) {
			return connection, result, err
		}
		err = nativeDispatchCause(lastErr, err)
		if ctx.Err() != nil {
			return nil, result, errors.Join(err, ctx.Err())
		}
		lastErr = err
		if err := reattach.Wait(ctx, min(native.AdmissionRetryInterval, time.Until(deadline))); err != nil {
			return nil, result, errors.Join(lastErr, err)
		}
	}
}

// nativeDispatchCause keeps the worker's answer (fenced, busy) over a later attempt's transport failure, which is
// what a caller's deadline makes of the attempt it cuts short.
func nativeDispatchCause(previous, err error) error {
	if previous != nil && errors.Is(err, ErrNativeDispatchUnavailable) {
		return previous
	}
	return err
}

func nativeDispatchTemporary(err error) bool {
	var network net.Error
	return errors.Is(err, hostdproto.ErrFenced) ||
		errors.Is(err, hostdproto.ErrMaintenanceBusy) ||
		errors.Is(err, ErrNativeDispatchUnavailable) ||
		errors.Is(err, ErrNativeDispatchBusy) || errors.As(err, &network)
}

func (c *NativeDispatchClient) callOnce(
	ctx context.Context, operation string, header *streamauth.Header,
	binding *nativeprivate.Binding, resume *NativeDispatchResumePeer, offline *NativeDispatchOfflinePeer, stream bool, deadline time.Time,
) (net.Conn, nativeDispatchResponse, error) {
	var result nativeDispatchResponse
	attemptDeadline := time.Now().Add(c.config.DialTimeout)
	if deadline.Before(attemptDeadline) {
		attemptDeadline = deadline
	}
	setupCtx, cancel := context.WithDeadline(ctx, attemptDeadline)
	defer cancel()
	envelope := nativeDispatchEnvelope{Binding: binding, Resume: resume, Offline: offline}
	var err error
	if header != nil {
		envelope.Header, err = header.MarshalBinary()
		if err != nil {
			return nil, result, err
		}
	}
	body, err := json.Marshal(envelope)
	if err != nil || len(body) > nativeDispatchMaximum {
		return nil, result, ErrStreamAuthorization
	}
	connection, err := (&net.Dialer{Timeout: c.config.DialTimeout}).DialContext(setupCtx, "tcp", c.config.Address)
	if err != nil {
		return nil, result, errors.Join(ErrNativeDispatchUnavailable, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	owned := false
	defer func() {
		if !owned {
			stop()
			connection.Close()
		}
	}()
	deadline, _ = setupCtx.Deadline()
	_ = connection.SetDeadline(deadline)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.config.Address+NativeDispatchPath+operation, bytes.NewReader(body))
	if err != nil {
		return nil, result, err
	}
	token := c.config.Token
	if len(token) < 32 || len(token) > 128 {
		return nil, result, ErrStreamAuthorization
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if err = request.Write(connection); err != nil {
		return nil, result, errors.Join(ErrNativeDispatchUnavailable, err)
	}
	limited := &io.LimitedReader{R: connection, N: 32 << 10}
	reader := bufio.NewReader(limited)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, result, errors.Join(ErrNativeDispatchUnavailable, err)
	}
	limited.N = 1<<63 - 1
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, result, nativeDispatchError(response.StatusCode)
	}
	if stream {
		// An upgraded private byte stream has no HTTP response body or mutation
		// retry. Its lifetime belongs to the caller's stream context.
		_ = connection.SetDeadline(time.Time{})
		owned = true
		return &nativeDispatchConn{Conn: connection, reader: reader, stop: stop}, result, nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, nativeDispatchMaximum+1))
	decoder.DisallowUnknownFields()
	var extra any
	err = decoder.Decode(&result)
	if err == nil && decoder.Decode(&extra) != io.EOF {
		err = ErrStreamAuthorization
	}
	response.Body.Close()
	if err != nil {
		return nil, result, errors.Join(ErrNativeDispatchUnavailable, err)
	}
	return nil, result, nil
}
func (c *NativeDispatchClient) Authorize(ctx context.Context, header streamauth.Header) (string, error) {
	_, response, err := c.call(ctx, "authorize", &header, nil, nil, nil, false)
	if err == nil && response.AccessSessionID == "" {
		err = ErrStreamAuthorization
	}
	if err != nil {
		reason := "dispatch_failed"
		switch {
		case errors.Is(err, hostdproto.ErrFenced):
			reason = "worker_fenced"
		case errors.Is(err, ErrStreamAuthorization):
			reason = "resource_denied"
		case errors.Is(err, ErrNativeDispatchUnavailable):
			reason = "worker_unavailable"
		case errors.Is(err, ErrNativeDispatchBusy):
			reason = "worker_busy"
		case errors.Is(err, context.Canceled):
			reason = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			reason = "deadline"
		}
		diagnosticlog.TryWarn("native dispatch admission refused", "reason", reason)
		code := native.AdmissionDenied
		if nativeDispatchTemporary(err) {
			code = native.AdmissionUnavailable
		}
		err = &native.AdmissionRefusal{Code: code, Err: err}
	}
	return response.AccessSessionID, err
}
func (c *NativeDispatchClient) Open(ctx context.Context, header streamauth.Header) (net.Conn, error) {
	if header.Resume != "" {
		// A resume carries no credential; only OpenResume vouches for its peer.
		recordNativeAdmissionFailure("resume_unwrapped", ErrStreamAuthorization)
		return nil, ErrStreamAuthorization
	}
	if header.Offline {
		// An offline session carries no credential; only OpenOffline vouches for its peer.
		recordNativeAdmissionFailure("offline_unwrapped", ErrStreamAuthorization)
		return nil, ErrStreamAuthorization
	}
	connection, _, err := c.call(ctx, "stream", &header, nil, nil, nil, true)
	return connection, err
}

// OpenOffline opens a credential-free terminal or exec stream the carrier admitted from its own signed scopes while
// Paperboat is unreachable (F21-11). The worker serves the stream as that peer's OfflinePeerConn.
func (c *NativeDispatchClient) OpenOffline(ctx context.Context, header streamauth.Header, peer NativeDispatchOfflinePeer) (net.Conn, error) {
	connection, _, err := c.call(ctx, "stream", &header, nil, nil, &peer, true)
	return connection, err
}

// OpenResume opens a credential-free terminal resume stream. The carrier already admitted peer from its current
// authority; the worker serves the stream as that peer's ResumePeerConn.
func (c *NativeDispatchClient) OpenResume(ctx context.Context, header streamauth.Header, peer NativeDispatchResumePeer) (net.Conn, error) {
	connection, _, err := c.call(ctx, "stream", &header, nil, &peer, nil, true)
	return connection, err
}
func (c *NativeDispatchClient) OpenPrivate(ctx context.Context, binding nativeprivate.Binding) (net.Conn, error) {
	connection, _, err := c.call(ctx, "private", nil, &binding, nil, nil, true)
	return connection, err
}
func (c *NativeDispatchClient) SSHTarget(ctx context.Context) (managedssh.HostTarget, error) {
	_, response, err := c.call(ctx, "ssh-target", nil, nil, nil, nil, false)
	if err != nil {
		return managedssh.HostTarget{}, err
	}
	if response.SSH == nil || !nativeDispatchSSHTargetValid(*response.SSH) {
		return managedssh.HostTarget{}, ErrStreamAuthorization
	}
	return *response.SSH, nil
}
func (c *NativeDispatchClient) Current(ctx context.Context, binding nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
	_, response, err := c.call(ctx, "current", nil, &binding, nil, nil, false)
	if err != nil {
		return time.Time{}, nil, err
	}
	if !response.ValidUntil.After(serverclock.Now()) {
		return time.Time{}, nil, ErrNativePrivateBinding
	}
	lease, err := admission.Admit(ctx, response.ValidUntil, nil)
	if err != nil {
		return time.Time{}, nil, err
	}
	lease.Poll(func(checkCtx context.Context) error {
		_, _, err := c.call(checkCtx, "current", nil, &binding, nil, nil, false)
		if errors.Is(err, ErrStreamAuthorization) || errors.Is(err, hostdproto.ErrFenced) {
			return admission.ErrRevoked
		}
		return err
	})
	revoked := lease.Context().Done()
	return response.ValidUntil, revoked, nil
}
