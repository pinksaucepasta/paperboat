package tailnet

import (
	"net/netip"
	"slices"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// Endpoint reports tell the server this engine's own candidate addresses, so peers can reach it directly with
// no relay (PLAN 3b "Direct paths without a relay"). A report rides on the configuration request from the first
// one, then debounced and spaced so a flapping network does not become a stream of requests.
const (
	endpointReportDebounce = 5 * time.Second
	endpointReportSpacing  = 30 * time.Second
	// endpointFirstReportHold bounds how long a new engine's first report waits for its first candidates. A carrier replaced
	// by an update has found none yet when its first renewal goes out; an empty report then tells every peer the machine
	// has no endpoint while the old generation's are still valid (Tailscale's magicsock sends no update before it has
	// gathered endpoints, and debounces the first one).
	endpointFirstReportHold = 2 * time.Second
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
		if !peernetwork.ValidPeerEndpoint(ep) {
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
	if a.reported == nil && current != nil && len(current) == 0 && a.holdFirstReportLocked() {
		return nil
	}
	// The first report goes out even when empty: a request carrying `endpoints` is how the server learns
	// this client accepts endpoints on delivered peer entries (agreed with Lane Q), and a machine with no
	// candidate must still receive its peers'.
	return &current
}

// holdFirstReportLocked reports whether the first report still waits for candidates: until endpointFirstReportHold has
// passed since this engine was first asked. A discovery asks for the report at once (endpointsChanged), and a timer
// asks when the hold ends, so a machine that really has no candidate still says so after the bound. reportMu is held.
func (a *Authority) holdFirstReportLocked() bool {
	if a.firstReportAsked.IsZero() {
		a.firstReportAsked = time.Now()
	}
	left := endpointFirstReportHold - time.Since(a.firstReportAsked)
	if left <= 0 {
		return false
	}
	if a.reportTimer == nil {
		a.reportTimer = time.AfterFunc(left, func() {
			a.reportMu.Lock()
			a.reportTimer = nil
			a.reportMu.Unlock()
			a.RequestRefresh()
		})
	}
	return true
}

// restunSpacing is magicsock's own periodic STUN interval (20-26 s, wgengine/magicsock/magicsock.go
// updateEndpoints): waking a peer at every retry of a waiting transfer must not out-run it.
const restunSpacing = 20 * time.Second

// ReSTUN re-learns this engine's own endpoints (magicsock netcheck, which also re-reports a change to the server
// and makes its next peer pings carry the fresh address), at most once per restunSpacing. A machine whose UDP
// came back after a block otherwise keeps the endpoints it learned while blocked (Tailscale: userspace.go
// linkChange and magicsock.go maybeRebindOnError both ReSTUN when a path stops working).
func (a *Authority) ReSTUN(why string) {
	a.mu.Lock()
	engine := a.clientEngine
	if engine == nil && a.server != nil {
		engine = a.server.server
	}
	due := engine != nil && time.Since(a.lastReSTUN) >= restunSpacing
	if due {
		a.lastReSTUN = time.Now()
	}
	a.mu.Unlock()
	if due {
		engine.ReSTUN(why)
	}
}

// endpointsReported records the set the server accepted.
func (a *Authority) endpointsReported(endpoints []string) {
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	a.reported = &endpoints
	a.lastReport = time.Now()
	a.startupReported = a.startupReported || len(endpoints) > 0
}

// endpointsChanged schedules one renewal that carries the new endpoints: endpointReportDebounce after the
// change, and at least endpointReportSpacing after the previous report. Further changes while one is pending
// ride on it.
func (a *Authority) endpointsChanged() {
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	// Initial reachability is not a steady-state address change. The urgent lane
	// still enforces server Retry-After and coalesces concurrent discoveries.
	if !a.startupReported {
		a.RequestUrgentRefresh()
		return
	}
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

// homeRelayReport is the node id of the regional relay this machine's listening engine is homed on (the first of
// its relay regions), or empty for a CLI or before a relay is set. Every renewal carries it, as a Tailscale node
// reports its PreferredDERP in each map request (control/controlclient); its change asks for one (endpointsChanged).
func (a *Authority) homeRelayReport() string {
	a.mu.Lock()
	var engine *mesh.Server
	if a.server != nil {
		engine = a.server.server
	}
	a.mu.Unlock()
	if engine == nil {
		return ""
	}
	home := engine.HomeRegion()
	a.relay.mu.Lock()
	defer a.relay.mu.Unlock()
	for _, node := range a.relay.nodes {
		if regionalID(node.NodeID) == home {
			return node.NodeID
		}
	}
	return ""
}
