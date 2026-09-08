package tailcat

import "tailscale.com/tailcfg"

// RelayTransport reports the concrete transport carrying a server region.
func (s *Server) RelayTransport(id tailcfg.DERPRegionID) string {
	if s.lb == nil {
		return ""
	}
	return s.lb.sys.MagicSock.Get().RelayTransport(id)
}

// RelayTransport reports the concrete transport carrying a client region.
func (c *Client) RelayTransport(id tailcfg.DERPRegionID) string {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if !c.started || c.lb == nil {
		return ""
	}
	return c.lb.sys.MagicSock.Get().RelayTransport(id)
}
