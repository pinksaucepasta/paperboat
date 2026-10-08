//go:build !linux && !darwin && !windows

package machineguard

import (
	"context"
	"errors"
)

var errLocalTrustUnsupported = errors.New("local HTTPS certificate trust is unsupported on this platform")

func installLocalTrust(context.Context, Config, []byte) error { return errLocalTrustUnsupported }
func cleanupHistoricalTrustExcept(context.Context, Config, []byte) error {
	return errLocalTrustUnsupported
}
func InstallUserTrust(context.Context, []byte) error { return errLocalTrustUnsupported }
func RemoveUserTrust(context.Context, []byte) error  { return errLocalTrustUnsupported }
func CleanupUserTrust(context.Context) error         { return errLocalTrustUnsupported }
