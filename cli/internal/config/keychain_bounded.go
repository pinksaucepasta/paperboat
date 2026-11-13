package config

import (
	"context"
	"time"
)

// keychainCallTimeout bounds one macOS Keychain call. `security` waits forever for a prompt that cannot appear
// to a user with no unlocked login keychain, and `pb pair` as a second OS user hung for 18 minutes (X-28).
var keychainCallTimeout = 30 * time.Second

// boundedKeychain runs call and gives up after keychainCallTimeout. The call cannot be cancelled, so a stuck
// `security` child is left to exit with pb (its stdin closes).
func boundedKeychain[T any](call func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call()
		done <- result{value, err}
	}()
	timer := time.NewTimer(keychainCallTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.value, r.err
	case <-timer.C:
		var zero T
		return zero, credentialStoreFailure("macOS Keychain did not answer in time; the login keychain is locked or missing for this user", context.DeadlineExceeded)
	}
}
