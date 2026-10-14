//go:build windows

package localapi

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

// Hijacked stream bridges observe local EOF themselves. Process exit is the
// additional ownership signal needed to terminate a stream whose client dies
// without a clean pipe close.
func watchPeerHangup(ctx context.Context, _ net.Conn, peer Peer, cancel context.CancelFunc) {
	processExit, closeProcessExit := watchProcessExit(peer.PID)
	defer closeProcessExit()
	select {
	case <-ctx.Done():
	case <-processExit:
		cancel()
	}
}

// A file-transfer-key lease deliberately has no byte bridge after its HTTP
// upgrade, so wait for either a process exit or pipe EOF. No application data
// is read from this control connection after the successful response.
func watchControlHangup(ctx context.Context, connection net.Conn, peer Peer, cancel context.CancelFunc) {
	processExit, closeProcessExit := watchProcessExit(peer.PID)
	defer closeProcessExit()
	closed := make(chan error, 1)
	go func() {
		var value [1]byte
		_, err := connection.Read(value[:])
		closed <- err
	}()
	select {
	case <-ctx.Done():
	case <-processExit:
		cancel()
	case err := <-closed:
		cancel()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			errorreport.Current().ObserveFailure(ctx, "paperboatd", "transfer", "local_gateway", "local_gateway_failed", err)
		}
		return
	}
	// This dedicated control connection carries no application bytes. Closing
	// it interrupts the owned reader, which is joined before releasing the lease.
	_ = connection.Close()
	<-closed
}
