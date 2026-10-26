package tailnet

import (
	"net/netip"
	"slices"
	"time"
)

// Endpoint reports tell the server this engine's own candidate addresses, so peers can reach it directly with
// no relay (PLAN 3b "Direct paths without a relay"). A report rides on the configuration request, only once
// the server announced endpoint_reports, debounced and spaced so a flapping network does not become a
// stream of requests.
const (
	endpointReportDebounce = 5 * time.Second
	endpointReportSpacing  = 30 * time.Second
)

// localEndpointReport is the engine's current candidate set as the server stores it: valid unicast ip:port,
// sorted, deduplicated, at most MaxPeerEndpoints.
func (a *Authority) localEndpointReport() []string {
	a.mu.Lock()
	engine := a.clientEngine
	if engine == nil && a.server != nil {
		engine = a.server.server
	}
	a.mu.Unlock()
	if engine == nil {
		return nil
	}
	var endpoints []netip.AddrPort
	for _, ep := range engine.LocalEndpoints() {
		ip := ep.Addr()
		if ep.Port() == 0 || !ip.IsValid() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		endpoints = append(endpoints, ep)
	}
	slices.SortFunc(endpoints, func(x, y netip.AddrPort) int { return x.Compare(y) })
	endpoints = slices.Compact(endpoints)
	report := make([]string, 0, min(len(endpoints), MaxPeerEndpoints))
	for _, ep := range endpoints[:min(len(endpoints), MaxPeerEndpoints)] {
		report = append(report, ep.String())
	}
	a.reportMu.Lock()
	a.lastReport = time.Now()
	a.reportMu.Unlock()
	return report
}

// endpointsChanged schedules one renewal that carries the new endpoints: endpointReportDebounce after the
// change, and at least endpointReportSpacing after the previous report. Further changes while one is pending
// ride on it.
func (a *Authority) endpointsChanged() {
	if !a.endpointReports.Load() {
		return
	}
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	if a.reportTimer != nil {
		return
	}
	wait := max(endpointReportDebounce, endpointReportSpacing-time.Since(a.lastReport))
	a.reportTimer = time.AfterFunc(wait, func() {
		a.reportMu.Lock()
		a.reportTimer = nil
		a.reportMu.Unlock()
		a.RequestRefresh()
	})
}
