package autoupdate

import (
	"fmt"
	"strings"
)

const BlockedActiveTerminalSessions = "active_terminal_sessions"

// ActiveTerminalSessionsError leaves an eligible release pending until its
// nonresumable workloads finish. It never grants a release-policy deferral.
// SSH and Forwards count the open SSH-family connections and local access forwards when those, not
// terminal sessions, are what the update is waiting for: replacing the daemon would end them.
type ActiveTerminalSessionsError struct {
	RequiredVersion string
	SSH, Forwards   int
}

func (e *ActiveTerminalSessionsError) Error() string {
	if e.SSH+e.Forwards == 0 {
		return "update is waiting for running or detached terminal sessions to finish"
	}
	var open []string
	if e.SSH > 0 {
		open = append(open, plural(e.SSH, "open SSH connection", "open SSH connections"))
	}
	if e.Forwards > 0 {
		open = append(open, plural(e.Forwards, "open port forward", "open port forwards"))
	}
	return "Paperboat is busy: " + strings.Join(open, " and ") + ". Wait, close them, or use --force."
}

func plural(count int, one, many string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, one)
	}
	return fmt.Sprintf("%d %s", count, many)
}
