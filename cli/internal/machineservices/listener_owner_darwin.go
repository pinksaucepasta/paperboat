//go:build darwin

package machineservices

// listenerOwner has no cheap implementation on macOS: the existing pcb-list
// walk (machineguard/pcblist.go) keys sockets by exact connecting tuple and
// reports the socket UID, not a pid, so it cannot answer "who LISTENs on
// :port" with a name; libproc/proc_pidfdinfo/proc_pidpath are used nowhere
// in the repo; and the lsof spawn in discovery_darwin.go (fork+exec, 2 s
// timeout) is too heavy and racy for a doctor hot path. Callers print
// "another program is using port 80 (process unknown on macOS)".
func listenerOwner(uint16) (string, int, bool) { return "", 0, false }
