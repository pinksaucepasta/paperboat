package hostservice

// stalePolicy reports whether a requested availability policy is older than the one this host holds.
// Version 0 is the server's default policy, which the host has not been told yet: the host starts at
// keep_awake version 0 so it stays reachable before the first policy arrives, and a machine whose owner
// never changed the setting is at allow_sleep version 0 on the server. So a version 0 request always
// replaces a version 0 state; otherwise a same-version request with another mode would be refused
// forever and the machine would never report its availability (console: "Waiting for <machine>").
func stalePolicy(mode string, version int64, current State) bool {
	if version == 0 && current.DesiredVersion == 0 {
		return false
	}
	return version < current.DesiredVersion || version == current.DesiredVersion && mode != current.DesiredMode
}
