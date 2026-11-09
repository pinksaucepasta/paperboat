package localdaemon

import "errors"

// ErrServiceNotInstalled means the local daemon's service is not registered with the operating
// system. Commands name pb service install, which registers it again.
var ErrServiceNotInstalled = errors.New("Paperboat local daemon service is not installed; run pb service install")

// ServiceState is the canonical local daemon's native service-manager state.
type ServiceState struct {
	Installed bool
	Running   bool
}
