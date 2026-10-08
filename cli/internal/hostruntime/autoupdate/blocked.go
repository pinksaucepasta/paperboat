package autoupdate

const BlockedActiveTerminalSessions = "active_terminal_sessions"

// ActiveTerminalSessionsError leaves an eligible release pending until its
// nonresumable workloads finish. It never grants a release-policy deferral.
type ActiveTerminalSessionsError struct {
	RequiredVersion string
}

func (e *ActiveTerminalSessionsError) Error() string {
	return "update to " + e.RequiredVersion + " is waiting for running or detached terminal sessions to finish"
}
