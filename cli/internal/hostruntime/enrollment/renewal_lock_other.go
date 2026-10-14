//go:build !darwin && !linux && !windows

package enrollment

import "errors"

type runtimeIdentityRenewalLock struct{}

func openRuntimeIdentityRenewalLock(string) (*runtimeIdentityRenewalLock, error) {
	return nil, errors.ErrUnsupported
}

func (*runtimeIdentityRenewalLock) Try() (bool, error) { return false, errors.ErrUnsupported }
func (*runtimeIdentityRenewalLock) Close() error       { return nil }
