package edgehttp

// IngressStats describes live dedicated connector attachments, not admitted
// sessions that have not connected or origin readiness.
type IngressStats struct {
	Sessions, Routes int
	ActiveStreams    uint32
}

func (r *DataCarrierRouteRegistry) Stats() IngressStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var stats IngressStats
	// Distinct ids: a route served by two carriers is one route, as in the registry diagnostics compares against.
	routes := map[string]struct{}{}
	for _, entry := range r.byKey {
		if entry.server == nil {
			continue
		}
		stats.Sessions++
		for _, id := range entry.state.RouteIDs {
			routes[id] = struct{}{}
		}
		stats.ActiveStreams += uint32(entry.server.ActiveStreams())
	}
	stats.Routes = len(routes)
	return stats
}
