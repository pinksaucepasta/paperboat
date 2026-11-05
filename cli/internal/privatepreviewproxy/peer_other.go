//go:build !linux && !darwin

package privatepreviewproxy

// shortcut: Windows has no peer attribution yet, so its proxy is not restricted to the owning user; add it
// with Windows support after launch.
func peerRefusalReason(error) string { return refusalLookupFailed }
