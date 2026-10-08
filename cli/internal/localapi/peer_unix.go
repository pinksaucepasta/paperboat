//go:build linux || darwin

package localapi

import (
	"errors"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/ospeer"
)

func peerIdentity(connection net.Conn) (Peer, error) {
	identity, err := ospeer.Get(connection)
	if err != nil {
		return Peer{}, errors.Join(ErrPermission, err)
	}
	return Peer{UID: identity.UID, GID: identity.GID, PID: identity.PID}, nil
}
