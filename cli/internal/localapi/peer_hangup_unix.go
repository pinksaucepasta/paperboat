//go:build darwin || linux

package localapi

import (
	"context"
	"errors"
	"net"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"golang.org/x/sys/unix"
)

func watchPeerHangup(ctx context.Context, connection net.Conn, peer Peer, cancel context.CancelFunc) {
	systemConnection, ok := connection.(syscall.Conn)
	if !ok {
		cancel()
		errorreport.Current().ObserveFailure(ctx, "paperboatd", "peer_stream", "local_gateway", "local_gateway_failed", ErrInvalidConfig)
		return
	}
	raw, err := systemConnection.SyscallConn()
	if err != nil {
		cancel()
		errorreport.Current().ObserveFailure(ctx, "paperboatd", "peer_stream", "local_gateway", "local_gateway_failed", err)
		return
	}
	processExit, closeProcessExit := watchProcessExit(peer.PID)
	defer closeProcessExit()
	for ctx.Err() == nil {
		select {
		case <-processExit:
			cancel()
			return
		default:
		}
		var count int
		var pollErr error
		var events int16
		controlErr := raw.Control(func(value uintptr) {
			// Retain the descriptor borrow throughout the bounded poll. A closed
			// descriptor cannot be reused by another connection during this wait.
			poll := []unix.PollFd{{Fd: int32(value), Events: unix.POLLHUP | unix.POLLERR}}
			count, pollErr = unix.Poll(poll, 250)
			events = poll[0].Revents
		})
		if controlErr != nil {
			cancel()
			if !normalPeerStreamTermination(controlErr) {
				errorreport.Current().ObserveFailure(ctx, "paperboatd", "peer_stream", "local_gateway", "local_gateway_failed", controlErr)
			}
			return
		}
		if errors.Is(pollErr, unix.EINTR) {
			continue
		}
		if pollErr != nil {
			cancel()
			errorreport.Current().ObserveFailure(ctx, "paperboatd", "peer_stream", "local_gateway", "local_gateway_failed", pollErr)
			return
		}
		if count > 0 && events&unix.POLLNVAL != 0 {
			cancel()
			errorreport.Current().ObserveFailure(ctx, "paperboatd", "peer_stream", "local_gateway", "local_gateway_failed", unix.EBADF)
			return
		}
		if count > 0 && events&(unix.POLLHUP|unix.POLLERR) != 0 {
			cancel()
			return
		}
	}
}

func watchControlHangup(ctx context.Context, connection net.Conn, peer Peer, cancel context.CancelFunc) {
	watchPeerHangup(ctx, connection, peer, cancel)
}
