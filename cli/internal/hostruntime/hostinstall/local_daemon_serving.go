package hostinstall

import "github.com/pinksaucepasta/paperboat/internal/localapi"

// localDaemonServing says the local daemon of this install is up: ready, or signed out (the account revoked this
// computer's sign-in) on a machine whose registration is on disk. An install replaces code and cannot sign anyone
// in, so a signed-out daemon must not fail the install and roll it back (winpool-7, 2026-11-12: install_readiness
// failed on "awaiting_enrollment" every time). A daemon without a registration is awaiting enrollment for real.
func localDaemonServing(state localapi.Snapshot, version string, registered bool) bool {
	if state.DaemonVersion != version {
		return false
	}
	if state.DaemonState == "ready" {
		return true
	}
	if state.DaemonState != "awaiting_enrollment" || !registered {
		return false
	}
	for _, item := range state.Health {
		if item.Code == "authentication_required" {
			return true
		}
	}
	return false
}
