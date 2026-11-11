//go:build windows

// Package winpipe dials the Paperboat services' named pipes.
package winpipe

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/windows/pipeauth"
	"golang.org/x/sys/windows"
)

// AbsentWait bounds how long Dial waits for a pipe with no server instance. A hostd generation handover re-binds
// the fixed pipes in sequence, so they are absent for milliseconds (W21 Part B); a pipe still absent after this is
// a stopped service and is reported as such.
const AbsentWait = 2 * time.Second

// Dial dials the named pipe at path, retrying every 20 ms for up to AbsentWait while no server instance exists
// (ERROR_FILE_NOT_FOUND). winio already waits on a busy pipe; every other error returns at once.
// Prior art: Tailscale safesocket.ConnectContext retries while tailscaled is still starting.
// The connected pipe must have been created by SYSTEM, Administrators, the caller or owner (the enrolled account
// whose process serves it, "" for a SYSTEM service), with a DACL that lets no other account serve it
// (pipeauth.CheckService); a pipe squatted by another account while the service is absent is refused before any
// byte is sent.
func Dial(ctx context.Context, path, owner string) (net.Conn, error) {
	deadline := time.Now().Add(AbsentWait)
	for {
		connection, err := winio.DialPipeContext(ctx, path)
		if err == nil {
			if err := Verify(connection, owner); err != nil {
				return nil, err
			}
			return connection, nil
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || !time.Now().Before(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(err, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Verify applies Dial's server check to a pipe connection dialed another way and closes it when the check fails.
func Verify(connection net.Conn, owner string) error {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return errors.Join(errors.New("resolve current Windows user"), err, connection.Close())
	}
	self := user.User.Sid.String()
	return pipeauth.VerifyConn(connection, func(descriptor *windows.SECURITY_DESCRIPTOR) error {
		return pipeauth.CheckService(descriptor, self, owner)
	})
}
