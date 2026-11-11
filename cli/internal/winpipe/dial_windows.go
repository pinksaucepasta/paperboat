//go:build windows

// Package winpipe dials the Paperboat services' named pipes.
package winpipe

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// AbsentWait bounds how long Dial waits for a pipe with no server instance. A hostd generation handover re-binds
// the fixed pipes in sequence, so they are absent for milliseconds (W21 Part B); a pipe still absent after this is
// a stopped service and is reported as such.
const AbsentWait = 2 * time.Second

// Dial dials the named pipe at path, retrying every 20 ms for up to AbsentWait while no server instance exists
// (ERROR_FILE_NOT_FOUND). winio already waits on a busy pipe; every other error returns at once.
// Prior art: Tailscale safesocket.ConnectContext retries while tailscaled is still starting.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	deadline := time.Now().Add(AbsentWait)
	for {
		connection, err := winio.DialPipeContext(ctx, path)
		if err == nil || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || !time.Now().Before(deadline) {
			return connection, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(err, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
