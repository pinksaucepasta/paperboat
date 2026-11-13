package usage

import "time"

// DeliveryWindow is the v1 operational report delivery and maximum metered
// stream lifetime, mirrored by the control plane accountingauthority policy.
// Older signed reports receive durable expired_not_billed acknowledgements;
// the producer removes them only after that acknowledgement.
const DeliveryWindow = 7 * 24 * time.Hour

// The usage limits live here and nowhere else. Every preview is a new route, so counters would
// otherwise grow with a week of churn: past MaxCounters the oldest already-delivered ones are
// evicted; if none is delivered the edge keeps serving and alerts.
const (
	QueueReports = 4096
	QueueBytes   = 64 << 20
	MaxCounters  = 16384
)
