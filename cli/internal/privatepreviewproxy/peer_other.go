//go:build !linux && !darwin && !windows

package privatepreviewproxy

// shortcut: Windows has no peer attribution yet, so its proxy is not restricted to the owning user; add it
// with Windows support after launch.
func platformOwnSID() string { return "" }

func peerRefusalReason(error) string { return refusalLookupFailed }
