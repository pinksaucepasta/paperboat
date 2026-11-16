//go:build darwin || linux || windows

package runtime

// RefreshNetwork only nudges the existing engine; replacing it would drop
// active sessions. Push also refreshes the bounded worker-health lease.
func (c *ProductionCarrier) RefreshNetwork() {
	c.events.Push(carrierRenewalTopics, c.events.Healthy())
}

func (c *ProductionCarrier) EventsHealth(healthy bool) { c.events.Push(nil, healthy) }
