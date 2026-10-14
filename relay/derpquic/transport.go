package derpquic

// Transport reports the concrete relay transport currently carrying traffic.
func (*Client) Transport() string { return "derp_quic" }

// Transport reports the concrete relay transport currently carrying traffic.
func (*WSSClient) Transport() string { return "derp_wss" }

// Transport reports the active fallback leg. An unconnected or closed carrier
// has no observed transport.
func (c *FallbackCarrier) Transport() string {
	active, err := c.current()
	if err != nil {
		return ""
	}
	reporter, ok := active.(interface{ Transport() string })
	if !ok {
		return ""
	}
	return reporter.Transport()
}
