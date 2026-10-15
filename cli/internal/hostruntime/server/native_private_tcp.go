package server

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-relay/admission"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

type NativePrivateTCPCurrent func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error)
type NativePrivateTCPDial func(context.Context, string, string) (net.Conn, error)

// ServeNativePrivateTCP checks host-owned current state immediately before
// dialing the exact signed loopback target, then closes both directions when
// access is revoked or the connection disconnects.
func ServeNativePrivateTCP(ctx context.Context, header streamauth.Header, client net.Conn, current NativePrivateTCPCurrent, dial NativePrivateTCPDial) error {
	if ctx == nil || client == nil || current == nil || dial == nil || header.Consumer != "private_tcp" {
		return ErrNativePrivateBinding
	}
	binding, err := nativeprivate.Decode([]byte(header.Target), time.Now().UTC())
	if err != nil || binding.Protocol != "tcp" || binding.TargetScheme != "tcp" {
		return ErrNativePrivateBinding
	}
	validUntil, revoked, err := current(ctx, binding)
	if err != nil {
		return classifyNativePrivateFailure("peer_authority", "peer_authority_failed", err)
	}
	if !validUntil.After(time.Now().UTC()) {
		return ErrNativePrivateBinding
	}
	if binding.ExpiresAt.Before(validUntil) {
		validUntil = binding.ExpiresAt
	}
	lease, err := admission.Admit(ctx, validUntil, revoked)
	if err != nil {
		return classifyNativePrivateFailure("peer_authority", "peer_authority_failed", err)
	}
	defer lease.Close()
	deadlineCtx := lease.Context()
	stopClient := lease.Bind(client)
	defer stopClient()
	select {
	case <-revoked:
		return ErrNativePrivateBinding
	default:
	}
	if err := deadlineCtx.Err(); err != nil {
		return classifyNativePrivateFailure("peer_authority", "peer_authority_failed", err)
	}
	origin, err := dial(deadlineCtx, "tcp", binding.TargetAddress)
	if err != nil {
		return classifyNativePrivateFailure("target_connect", "native_private_failed", err)
	}
	defer origin.Close()
	stopOrigin := lease.Bind(origin)
	defer stopOrigin()
	if _, err = client.Write([]byte{0}); err != nil {
		return classifyNativePrivateFailure("stream_open", "native_private_failed", err)
	}
	return classifyNativePrivateFailure("delivery", "native_private_failed", bridgeNativePrivateTCP(client, origin))
}

func bridgeNativePrivateTCP(left, right net.Conn) error {
	results := make(chan error, 2)
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = left.Close(); _ = right.Close() }) }
	copyOne := func(destination, source net.Conn) {
		_, err := io.Copy(destination, source)
		if err == nil {
			if closer, ok := destination.(interface{ CloseWrite() error }); ok {
				err = closer.CloseWrite()
			} else {
				closeBoth()
			}
		}
		// Record the primary failure before closing the other direction can
		// produce a secondary closed-connection error.
		results <- err
		if err != nil {
			closeBoth()
		}
	}
	go copyOne(left, right)
	go copyOne(right, left)
	return errors.Join(<-results, <-results)
}
