package usage

import "time"

// DeliveryWindow is the v1 operational report delivery and maximum metered
// stream lifetime, mirrored by the control plane accountingauthority policy.
// Older signed reports receive durable expired_not_billed acknowledgements;
// the producer removes them only after that acknowledgement.
const DeliveryWindow = 7 * 24 * time.Hour
