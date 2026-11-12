//go:build windows

package managedssh

// InstalledAgentSocket returns the agent endpoint written to the local
// daemon's managed OpenSSH configuration: one pipe per Windows user, because a
// host-wide name let one user's daemon lock every other user out of managed SSH.
func InstalledAgentSocket(string) (string, error) {
	return ownerAgentSocket("")
}
