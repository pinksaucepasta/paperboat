package server

import (
	"context"
	"errors"
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
// that authority expires or is revoked.
func ServeNativePrivateTCP(ctx context.Context, header streamauth.Header, client net.Conn, current NativePrivateTCPCurrent, dial NativePrivateTCPDial) error {
	if ctx == nil || client == nil || current == nil || dial == nil || header.Consumer != "private_tcp" {
		return ErrNativePrivateBinding
	}
	binding, err := nativeprivate.Decode([]byte(header.Target), time.Now().UTC())
	if err != nil || binding.Protocol != "tcp" || binding.TargetScheme != "tcp" {
		return ErrNativePrivateBinding
	}
	validUntil, revoked, err := current(ctx, binding)
	if err != nil || !validUntil.After(time.Now().UTC()) {
		return ErrNativePrivateBinding
	}
	if binding.ExpiresAt.Before(validUntil) {
		validUntil = binding.ExpiresAt
	}
	origin, err := dial(ctx, "tcp", binding.TargetAddress)
	if err != nil {
		return err
	}
	defer origin.Close()
	if _, err = client.Write([]byte{0}); err != nil {
		return err
	}
	runCtx, cancel := context.WithDeadline(ctx, validUntil)
	defer cancel()
	go func() {
		select {
		case <-runCtx.Done():
		case <-revoked:
			cancel()
		}
		_ = client.Close()
		_ = origin.Close()
	}()
	return bridgeNativePrivateTCP(client, origin)
}

func bridgeNativePrivateTCP(left, right net.Conn) error {
	results := make(chan error, 2)
	var once sync.Once
	copyOne := func(destination, source net.Conn) {
		_, err := io.Copy(destination, source)
		if closer, ok := destination.(interface{ CloseWrite() error }); ok {
			err = errors.Join(err, closer.CloseWrite())
		} else {
			once.Do(func() { _ = destination.Close() })
		}
		results <- err
	}
	go copyOne(left, right)
	go copyOne(right, left)
	return errors.Join(<-results, <-results)
}
