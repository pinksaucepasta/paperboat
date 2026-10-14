package localdaemon

// ServiceState is the canonical local daemon's native service-manager state.
type ServiceState struct {
	Installed bool
	Running   bool
}
