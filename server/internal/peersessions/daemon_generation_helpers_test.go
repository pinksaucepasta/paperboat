package peersessions

import "github.com/pinksaucepasta/paperboat/shared/peernetwork"

const testDaemonGeneration = "0123456789abcdef0123456789abcdef"

// daemonEndpoint is the network endpoint of the test daemon generation of a CLI session.
func daemonEndpoint(session string) string {
	endpoint, ok := peernetwork.DaemonEndpointID(session, testDaemonGeneration)
	if !ok {
		panic("invalid test daemon endpoint")
	}
	return endpoint
}
