package connectorprotocol

import "testing"

func TestDataCarrierDefaultsAreWithinTheirLimits(t *testing.T) {
	if DataCarrierDefaultStreams > DataCarrierMaxStreams || DataCarrierDefaultAcceptBacklog > DataCarrierMaxAcceptBacklog || DataCarrierDefaultQueueDepth > DataCarrierMaxQueueDepth ||
		DataCarrierDefaultStreamWindow > DataCarrierMaxConfiguredWindow || DataCarrierDefaultStreamWindow > DataCarrierMaxStreamWindow || DataCarrierDefaultKeepAlive > DataCarrierMaxKeepAlive ||
		DataCarrierDefaultWriteLimit > DataCarrierMaxOperationTimeout || DataCarrierDefaultOpenLimit > DataCarrierMaxOperationTimeout || DataCarrierDefaultCloseLimit > DataCarrierMaxOperationTimeout {
		t.Fatal("a data-carrier default exceeds its limit")
	}
}
