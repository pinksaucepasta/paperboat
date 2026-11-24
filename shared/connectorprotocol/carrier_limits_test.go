package connectorprotocol

import "testing"

func TestDataCarrierDefaultsAreWithinTheirLimits(t *testing.T) {
	if DataCarrierDefaultStreams > DataCarrierMaxStreams || DataCarrierDefaultAcceptBacklog > DataCarrierMaxAcceptBacklog || DataCarrierDefaultQueueDepth > DataCarrierMaxQueueDepth ||
		DataCarrierDefaultStreamWindow > DataCarrierMaxConfiguredWindow || DataCarrierDefaultStreamWindow > DataCarrierMaxStreamWindow || DataCarrierDefaultKeepAlive > DataCarrierMaxKeepAlive ||
		DataCarrierDefaultWriteLimit > DataCarrierMaxOperationTimeout || DataCarrierDefaultOpenLimit > DataCarrierMaxOperationTimeout || DataCarrierDefaultCloseLimit > DataCarrierMaxOperationTimeout {
		t.Fatal("a data-carrier default exceeds its limit")
	}
}

// One machine serves 10,000 visitors at once over one carrier.
func TestOneCarrierCarriesTenThousandStreams(t *testing.T) {
	if DataCarrierDefaultStreams < 10000 {
		t.Fatalf("a carrier opens only %d streams at once", DataCarrierDefaultStreams)
	}
}
