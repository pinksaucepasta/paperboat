package windowsopenssh

import (
	"errors"
	"fmt"
	"net"
)

// CheckPortAvailable probes the single configured managed-SSH listener before
// enrollment is issued. A collision fails setup without mutating another
// user's service or registering an unreachable endpoint.
func CheckPortAvailable(port uint16) error {
	if port == 0 {
		return ErrInvalidConfig
	}
	v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("managed SSH port %d is already in use; remove the conflicting local listener and retry setup: %w", port, err)
	}
	v6, err6 := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	closeErr := v4.Close()
	if err6 != nil {
		return errors.Join(fmt.Errorf("managed SSH port %d is already in use; remove the conflicting local listener and retry setup: %w", port, err6), closeErr)
	}
	return errors.Join(v6.Close(), closeErr)
}
