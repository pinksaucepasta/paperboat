package magicsock

import "tailscale.com/tailcfg"

// RelayTransport reports the concrete transport used by an active regional
// carrier. Carriers that don't expose transport identity return an empty value.
func (c *Conn) RelayTransport(id tailcfg.DERPRegionID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	active, ok := c.activeDerp[id]
	if !ok {
		return ""
	}
	reporter, ok := active.c.(interface{ Transport() string })
	if !ok {
		return ""
	}
	return reporter.Transport()
}
