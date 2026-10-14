//go:build !darwin && !linux && !windows

package managedssh

import (
	"errors"
	"net"
)

func ListenOwnerSocket(string) (net.Listener, error) {
	err := errors.New("managed SSH agent sockets are unsupported on this platform")
	return nil, managedSSHFailure("listener_bind", err, err)
}
