//go:build linux || darwin

package hostservice

import (
	"net"

	"github.com/pinksaucepasta/paperboat/cli/internal/ospeer"
)

func peerUID(connection *net.UnixConn) (int, error) {
	identity, err := ospeer.Get(connection)
	if err != nil {
		return -1, err
	}
	return identity.UID, nil
}
