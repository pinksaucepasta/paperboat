//go:build linux

package handover

import (
	"fmt"
	"net"
	"syscall"
)

// requirePeerUID refuses a handover peer that is not the given user (SO_PEERCRED). Certificate
// private keys cross this socket, so it is checked before anything is read or sent, not only by the
// socket file's 0600 mode.
func requirePeerUID(conn *net.UnixConn, uid int) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("%w: peer credentials: %w", ErrHandover, err)
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return fmt.Errorf("%w: peer credentials: %w", ErrHandover, err)
	}
	if credErr != nil {
		return fmt.Errorf("%w: peer credentials: %w", ErrHandover, credErr)
	}
	if int(cred.Uid) != uid {
		return fmt.Errorf("%w: peer user %d is not user %d", ErrHandover, cred.Uid, uid)
	}
	return nil
}
