package terminalprotocol

import (
	"strconv"
	"time"
)

// StallAfter is how long a host may send nothing before a client flags the terminal as stalled. Hosts send a
// heartbeat every 5 s (hostruntime terminalHeartbeatInterval); two missed ones, like EternalTerminal's 11 s
// bound, mean the link is gone. pb connect and the browser terminal both use it.
const StallAfter = 10 * time.Second

// ReplayGapRejoins bounds how often a client rejoins at the live boundary after replay_gap: a busy terminal
// can compact past the new cursor before the retry arrives.
const ReplayGapRejoins = 3

// ReplayGapMarker is written once into the terminal when retained output no longer reaches the client's cursor.
const ReplayGapMarker = "\r\n[paperboat] Earlier terminal output is unavailable; showing retained output.\r\n"

// ExitCode is the shell-style status of a finished process: its code, or 128+N for a signal (SIGTERM is 143).
func ExitCode(code int, signal string) int {
	if code != 0 || signal == "" {
		return code
	}
	switch signal {
	case "SIGHUP":
		return 129
	case "SIGINT":
		return 130
	case "SIGKILL":
		return 137
	case "SIGTERM":
		return 143
	default:
		return 1
	}
}

// ExitStatus words a process exit for people: "exited 0", "exited 143 (SIGTERM)".
func ExitStatus(code int, signal string) string {
	status := "exited " + strconv.Itoa(ExitCode(code, signal))
	if signal != "" {
		status += " (" + signal + ")"
	}
	return status
}
