package peernetwork

// DaemonEndpointID is the network endpoint of one user daemon generation of a CLI session: every installed pb
// binary runs its own generation (named by the binary's 32-hex SHA-256 prefix, like the carrier units), so an
// upgraded daemon has its own WireGuard key and address while the old one drains what it holds. It reports false
// for a generation that is not 32 lowercase hex.
func DaemonEndpointID(cliSessionID, generation string) (string, bool) {
	if cliSessionID == "" || !ValidDaemonGeneration(generation) {
		return "", false
	}
	return cliSessionID + "_" + generation, true
}

// ValidDaemonGeneration reports whether generation is a daemon generation name: 32 lowercase hex.
func ValidDaemonGeneration(generation string) bool {
	if len(generation) != 32 {
		return false
	}
	for _, c := range generation {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
