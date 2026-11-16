package carriercontrol

import "context"

// RefreshNetwork forwards a worker's access or peer-network wake. Like SSH
// policy changes, event pushes are fenced by the active worker epoch.
func (c *Client) RefreshNetwork(ctx context.Context) error {
	return c.callWorker(ctx, "refresh-network")
}

// EventsHealth reports the worker's server-stream health. Successful reports
// also renew the carrier's bounded worker heartbeat lease.
func (c *Client) EventsHealth(ctx context.Context, healthy bool) error {
	if healthy {
		return c.callWorker(ctx, "recovered")
	}
	return c.callWorker(ctx, "events-unhealthy")
}
