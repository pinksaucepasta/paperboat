package tunnel

import "github.com/pinksaucepasta/paperboat/shared/peernetwork"

const testDaemonGeneration = "0123456789abcdef0123456789abcdef"

func testDaemonEndpoint(session string) string {
	endpoint, ok := peernetwork.DaemonEndpointID(session, testDaemonGeneration)
	if !ok {
		panic("invalid test daemon endpoint")
	}
	return endpoint
}
