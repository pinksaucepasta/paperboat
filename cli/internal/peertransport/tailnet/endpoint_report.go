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
		// The server refuses the whole request for one entry it would not store (Lane Q, 917095b6), so
		// keep only what it keeps: IPv4-mapped unwrapped, no loopback, unspecified, multicast or zoned address.
		ep = netip.AddrPortFrom(ep.Addr().Unmap(), ep.Port())
		if !reportableEndpoint(ep) {
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
	return report
}

// endpointReportDue is the endpoints this renewal reports: nil (field omitted) while they equal the last
// accepted report, else the current set, which may be empty: a machine whose candidates are gone says so,
// so peers stop dialing stale ones (Lane Q: [] stores "no candidates").
func (a *Authority) endpointReportDue() *[]string {
	current := a.localEndpointReport()
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	if a.reported != nil && slices.Equal(*a.reported, current) {
		return nil
	}
	// The first report goes out even when empty: a request carrying `endpoints` is how the server learns
	// this client accepts endpoints on delivered peer entries (agreed with Lane Q), and a machine with no
	// candidate must still receive its peers'.
	return &current
}

// endpointsReported records the set the server accepted.
func (a *Authority) endpointsReported(endpoints []string) {
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	a.reported = &endpoints
	a.lastReport = time.Now()
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
