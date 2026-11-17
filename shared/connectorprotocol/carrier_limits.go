package connectorprotocol

import "time"

// Data-carrier limits and defaults, one definition for the machine and the edge. One carrier serves every
// publication on a machine, so its stream bound must not refuse a popular preview's page loads (edge abuse plan,
// section 5). The Max* values bound what a configuration may ask for; DataCarrierMaxStreamWindow (carrier_window.go)
// is the separate auto-tuning ceiling of a stream's flow-control window.
const (
	DataCarrierDefaultStreams       = 1024
	DataCarrierDefaultAcceptBacklog = 16
	DataCarrierDefaultQueueDepth    = 16
	DataCarrierDefaultStreamWindow  = 256 << 10
	DataCarrierDefaultKeepAlive     = 30 * time.Second
	DataCarrierDefaultWriteLimit    = 10 * time.Second
	DataCarrierDefaultOpenLimit     = 10 * time.Second
	DataCarrierDefaultCloseLimit    = 5 * time.Second

	DataCarrierMaxStreams          = 1024
	DataCarrierMaxAcceptBacklog    = 1024
	DataCarrierMaxQueueDepth       = 1024
	DataCarrierMaxConfiguredWindow = 16 << 20
	DataCarrierMaxKeepAlive        = time.Hour
	DataCarrierMaxOperationTimeout = time.Minute
)
