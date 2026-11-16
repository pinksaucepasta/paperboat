//go:build darwin

package machineguard

// The macOS guard re-acquires its gateway ports from the daemons after a replacement.
func storeGateway(*sharedGateway) error { return nil }
func forgetGateway(*sharedGateway)      {}
