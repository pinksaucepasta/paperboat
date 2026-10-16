package edgehttp

import "github.com/pinksaucepasta/paperboat-tunnel/internal/route"

func carrierForRoute(entry *dataCarrierRouteEntry, rule route.RouteRule) bool {
	if entry == nil || entry.server == nil || entry.identity.AccountID != rule.AccountID || entry.identity.TunnelID != rule.TunnelID {
		return false
	}
	select {
	case <-entry.server.Done():
		return false
	default:
		return true
	}
}

// The route matcher retains a representative assignment. Every eligible replica
// must carry its own server-issued binding for the same immutable route/config.
// Capacity is checked when opening a stream, not when identifying TLS ownership.
func readyRouteReplica(entry *dataCarrierRouteEntry, rule route.RouteRule) bool {
	if !carrierForRoute(entry, rule) {
		return false
	}
	routeID := carrierRouteID(rule)
	binding, ok := replicaRouteBinding(entry.state.RouteBindings, routeID)
	return entry.state.Ready && entry.state.Generation == rule.ConfigGeneration &&
		entry.identity.Generation == rule.ConfigGeneration &&
		sortedContains(entry.state.RouteIDs, routeID) && sortedContains(entry.state.HealthyRoutes, routeID) &&
		ok && binding.RouteGeneration == rule.RouteGeneration && binding.ConfigContentHash == rule.ConfigContentHash
}
