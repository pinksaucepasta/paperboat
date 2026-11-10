//go:build !windows

package windowsopenssh

// TerminateManagedSessions ends nothing off Windows: OpenSSH there ends a session with its closed connection.
func TerminateManagedSessions(string) error { return nil }
