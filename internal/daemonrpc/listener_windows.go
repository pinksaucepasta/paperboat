//go:build windows

package daemonrpc

import (
	"fmt"
	"net"
	"os/user"
	"strings"

	"github.com/Microsoft/go-winio"
)

// Listen creates a Windows named pipe listener for daemon gRPC IPC.
func Listen(addr string) (net.Listener, error) {
	if addr == "" {
		addr = DefaultSocketAddress()
	}
	owner, err := user.Current()
	if err != nil || !strings.HasPrefix(owner.Uid, "S-1-") {
		return nil, fmt.Errorf("resolve daemon IPC owner")
	}
	for _, r := range strings.TrimPrefix(owner.Uid, "S-") {
		if r != '-' && (r < '0' || r > '9') {
			return nil, fmt.Errorf("invalid daemon IPC owner")
		}
	}
	listener, err := winio.ListenPipe(addr, &winio.PipeConfig{SecurityDescriptor: "O:" + owner.Uid + "D:P(A;;GA;;;SY)(A;;GA;;;" + owner.Uid + ")"})
	if err != nil {
		return nil, fmt.Errorf("listen on named pipe %s: %w", addr, err)
	}
	return listener, nil
}
