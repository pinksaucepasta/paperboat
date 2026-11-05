//go:build !linux

package handover

import (
	"fmt"
	"net"
)

// requirePeerUID cannot verify a peer here, so no handover happens (the edge runs on Linux).
func requirePeerUID(*net.UnixConn, int) error {
	return fmt.Errorf("%w: peer credentials are unavailable on this platform", ErrHandover)
}
