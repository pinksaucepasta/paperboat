package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

type NativeDispatchClientConfig struct {
	Address, Token string
	Active         func(context.Context) (hostdproto.Status, error)
	DialTimeout    time.Duration
}
type NativeDispatchClient struct{ config NativeDispatchClientConfig }

func NewNativeDispatchClient(config NativeDispatchClientConfig) (*NativeDispatchClient, error) {
	host, _, err := net.SplitHostPort(config.Address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || len(config.Token) < 32 || len(config.Token) > 128 || config.Active == nil || config.DialTimeout <= 0 || config.DialTimeout > 30*time.Second {
		return nil, ErrStreamAuthorization
	}
	return &NativeDispatchClient{config: config}, nil
}

type nativeDispatchConn struct {
	net.Conn
	reader *bufio.Reader
	stop   func() bool
}

func (c *nativeDispatchConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
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
func (c *NativeDispatchClient) call(ctx context.Context, operation string, header *streamauth.Header, binding *nativeprivate.Binding, stream bool) (net.Conn, nativeDispatchResponse, error) {
	var result nativeDispatchResponse
	if c == nil || ctx == nil {
		return nil, result, ErrStreamAuthorization
	}
	active, err := c.config.Active(ctx)
	if err != nil {
		return nil, result, err
	}
	if active.State != hostdproto.StateActive || active.WorkerID == "" || active.Epoch == 0 {
		return nil, result, hostdproto.ErrFenced
	}
	envelope := nativeDispatchEnvelope{WorkerID: active.WorkerID, Epoch: active.Epoch, Binding: binding}
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
	connection, err := (&net.Dialer{Timeout: c.config.DialTimeout}).DialContext(ctx, "tcp", c.config.Address)
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
	deadline := time.Now().Add(c.config.DialTimeout)
	if bound, ok := ctx.Deadline(); ok && bound.Before(deadline) {
		deadline = bound
	}
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
	_, response, err := c.call(ctx, "authorize", &header, nil, false)
	if err == nil && response.AccessSessionID == "" {
		err = ErrStreamAuthorization
	}
	return response.AccessSessionID, err
}
func (c *NativeDispatchClient) Open(ctx context.Context, header streamauth.Header) (net.Conn, error) {
	connection, _, err := c.call(ctx, "stream", &header, nil, true)
	return connection, err
}
func (c *NativeDispatchClient) OpenPrivate(ctx context.Context, binding nativeprivate.Binding) (net.Conn, error) {
	connection, _, err := c.call(ctx, "private", nil, &binding, true)
	return connection, err
}
func (c *NativeDispatchClient) SSHTarget(ctx context.Context) (managedssh.HostTarget, error) {
	_, response, err := c.call(ctx, "ssh-target", nil, nil, false)
	if err != nil {
		return managedssh.HostTarget{}, err
	}
	if response.SSH == nil || !nativeDispatchSSHTargetValid(*response.SSH) {
		return managedssh.HostTarget{}, ErrStreamAuthorization
	}
	return *response.SSH, nil
}
func (c *NativeDispatchClient) Current(ctx context.Context, binding nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
	_, response, err := c.call(ctx, "current", nil, &binding, false)
	if err != nil {
		return time.Time{}, nil, err
	}
	if !response.ValidUntil.After(time.Now()) {
		return time.Time{}, nil, ErrNativePrivateBinding
	}
	lease, err := admission.Admit(ctx, response.ValidUntil, nil)
	if err != nil {
		return time.Time{}, nil, err
	}
	lease.Poll(func(checkCtx context.Context) error {
		_, _, err := c.call(checkCtx, "current", nil, &binding, false)
		if errors.Is(err, ErrStreamAuthorization) || errors.Is(err, hostdproto.ErrFenced) {
			return admission.ErrRevoked
		}
		return err
	})
	revoked := lease.Context().Done()
	return response.ValidUntil, revoked, nil
}
