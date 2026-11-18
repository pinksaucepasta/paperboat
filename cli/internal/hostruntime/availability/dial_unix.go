//go:build darwin || linux

package availability

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"
)

func NewHostClient(socketPath string, timeout time.Duration) (*HostClient, error) {
	if !strings.HasPrefix(socketPath, "/") || timeout <= 0 {
		return nil, ErrInvalid
	}
	return &HostClient{socketPath: socketPath, timeout: timeout}, nil
}

func dialAvailabilityHostService(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "unix", path)
}

// closeAvailabilityHostServiceWrite tells the service the request is complete. A service that answered and closed
// before this runs has nothing left to hear: macOS then reports ENOTCONN from shutdown, while the buffered
// response is still readable, so it is not a failure.
func closeAvailabilityHostServiceWrite(connection net.Conn) error {
	err := connection.(interface{ CloseWrite() error }).CloseWrite()
	if errors.Is(err, syscall.ENOTCONN) {
		return nil
	}
	return err
}
