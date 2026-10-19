package tunnelmanager

import (
	"context"
	"fmt"
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
)

func (m *Manager) ResourceCounts() map[string]uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]uint64{"tunnels": uint64(len(m.active))}
}

// WorkloadIdentities supplies stable identity and generation fencing to hostd.
// Counts alone cannot distinguish one tunnel being replaced by another.
func (m *Manager) WorkloadIdentities() []string {
	if m == nil {
		return nil
	}
	active := m.snapshotActive()
	result := make([]string, 0, len(active))
	for _, value := range active {
		result = append(result, fmt.Sprintf("%s\x00%s\x00%d\x00%s", value.TunnelID(), value.ConnectorID(), value.Generation(), value.ContentHash()))
	}
	sort.Strings(result)
	return result
}

func (m *Manager) activeFor(tunnelID string) Active {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active[tunnelID]
}

// activeMatchesControlSession reports whether a published carrier is still
// bound to the latest authenticated control session which supplied the
// desired snapshot. No binding is recorded for ordinary restart recovery, so
// legacy/test candidates retain the existing same-generation no-op behavior.
func (m *Manager) activeMatchesControlSession(tunnelID string, active Active) bool {
	if m == nil || active == nil {
		return false
	}
	m.mu.RLock()
	binding, bound := m.sessionBindings[tunnelID]
	m.mu.RUnlock()
	if !bound {
		return true
	}
	if active.ConnectorID() != binding.ConnectorID || active.Generation() != binding.Generation || active.ContentHash() != binding.ContentHash {
		return false
	}
	provider, ok := active.(ActiveCarrierProvider)
	// Non-data-carrier candidates (legacy runtimes and deterministic manager
	// fakes) have no session identity to refresh. Preserve their existing
	// same-generation no-op behavior; the production DataCarrier runtime
	// implements ActiveCarrierProvider and is checked below.
	if !ok || provider.ActiveDataCarrier() == nil {
		return true
	}
	identity, ok := provider.ActiveDataCarrier().Identity()
	if !ok {
		return false
	}
	return identity.AccountID == binding.AccountID && identity.TunnelID == tunnelID && identity.ConnectorID == binding.ConnectorID && identity.SessionID == binding.SessionID && identity.ProcessGeneration == binding.ProcessGeneration && identity.Generation == binding.Generation
}

// ActiveForTunnel returns the exact currently published runtime. The boolean
// distinguishes an absent tunnel from a nil handle and allows adapters to
// apply generation fencing before attaching route-specific consumers.
func (m *Manager) ActiveForTunnel(tunnelID string) (Active, bool) {
	if m == nil || tunnelID == "" {
		return nil, false
	}
	m.mu.RLock()
	active, ok := m.active[tunnelID]
	m.mu.RUnlock()
	return active, ok && active != nil
}

// ValidateNativePrivateTarget verifies a server-bound native target against
// both durable last-known-good state and the exact currently published
// runtime. A grant alone never makes an obsolete origin current.
func (m *Manager) ValidateNativePrivateTarget(binding nativeprivate.Binding) error {
	if m == nil || binding.ValidateShape() != nil || binding.ResourceKind != "tunnel" || binding.OwnerEndpointID != m.config.HostID || binding.TargetGeneration != binding.RouteGeneration {
		return ErrOriginUnavailable
	}
	active, ok := m.ActiveForTunnel(binding.ResourceID)
	if !ok || active.Generation() != binding.ResourceGeneration {
		return ErrOriginUnavailable
	}
	state, _, err := m.config.Store.Snapshot()
	if err != nil {
		return err
	}
	var tunnel *hoststate.Tunnel
	for i := range state.Tunnels {
		if state.Tunnels[i].ID == binding.ResourceID {
			tunnel = &state.Tunnels[i]
			break
		}
	}
	if tunnel == nil || tunnel.DesiredState != "active" || tunnel.AppliedGeneration != binding.ResourceGeneration || tunnel.LastKnownGood == nil || tunnel.LastKnownGood.Generation != binding.ResourceGeneration {
		return ErrOriginUnavailable
	}
	decoded, err := hoststate.ParseTunnelConfigSnapshot(tunnel.LastKnownGood.Payload, tunnel.ID, tunnel.LastKnownGood.Generation)
	if err != nil {
		return err
	}
	if decoded.AccessMode != "private" {
		return ErrOriginUnavailable
	}
	var route *hoststate.TunnelConfigRoute
	for i := range decoded.Routes {
		if decoded.Routes[i].ID == binding.RouteID {
			route = &decoded.Routes[i]
			break
		}
	}
	wantProtocol := "http"
	if binding.Protocol == "tcp" {
		wantProtocol = "private_tcp"
	}
	if route == nil || route.DesiredState != "active" || route.Protocol != wantProtocol || route.OriginScheme != binding.TargetScheme || route.OriginAddress != binding.TargetAddress {
		return ErrOriginUnavailable
	}
	for _, generation := range state.RouteGenerations {
		if generation.TunnelID == binding.ResourceID && generation.RouteID == binding.RouteID && generation.Generation == binding.RouteGeneration {
			return nil
		}
	}
	return ErrOriginUnavailable
}

// ActiveSnapshot returns a point-in-time copy of the manager's published
// runtimes. The returned map is owned by the caller.
func (m *Manager) ActiveSnapshot() map[string]Active {
	if m == nil {
		return nil
	}
	return m.snapshotActive()
}

func (m *Manager) snapshotActive() map[string]Active {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]Active, len(m.active))
	for key, value := range m.active {
		result[key] = value
	}
	return result
}

func (m *Manager) swapActiveIfRunning(tunnelID string, active Active, ctx context.Context) (Active, bool, error) {
	m.mu.Lock()
	if !m.started || m.closed {
		m.mu.Unlock()
		return nil, false, nil
	}
	if expected, ok := ctx.Value(networkGenerationContextKey{}).(networkGenerationContext); ok {
		if expected.TunnelID != tunnelID || !m.networkGenerationCurrentLocked(expected.TunnelID, expected.Generation) {
			m.mu.Unlock()
			return nil, false, ErrGenerationConflict
		}
		state := m.networkStates[expected.TunnelID]
		if state.InFlight != expected.Generation || state.Applied >= expected.Generation {
			m.mu.Unlock()
			return nil, false, ErrGenerationConflict
		}
	}
	old := m.active[tunnelID]
	m.active[tunnelID] = active
	if expected, ok := ctx.Value(networkGenerationContextKey{}).(networkGenerationContext); ok {
		state := m.networkStates[expected.TunnelID]
		// Mark the network generation only after the active pointer has been
		// published under the same lock. A retry after an ambiguous response can
		// therefore return this exact active without staging another carrier.
		state.Applied = expected.Generation
		state.InFlight = 0
		m.networkStates[expected.TunnelID] = state
	}
	observer := m.config.ActiveObserver
	m.mu.Unlock()
	if observer != nil {
		observer(ActiveChange{TunnelID: tunnelID, Previous: old, Current: active})
	}
	return old, true, nil
}

func (m *Manager) acceptGeneration(tunnelID string, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if previous := m.seen[tunnelID]; previous > generation {
		return false
	}
	if generation > m.seen[tunnelID] {
		m.seen[tunnelID] = generation
	}
	return true
}

// purgeRemovedInspectorRoutes purges captures for previously live routes that
// the replacement no longer serves. Unchanged routes keep their history.
func (m *Manager) purgeRemovedInspectorRoutes(oldRoutes, newRoutes []string) {
	if m == nil || m.config.InspectorPurge == nil || len(oldRoutes) == 0 {
		return
	}
	live := make(map[string]struct{}, len(newRoutes))
	for _, id := range newRoutes {
		live[id] = struct{}{}
	}
	var removed []string
	for _, id := range oldRoutes {
		if _, ok := live[id]; !ok {
			removed = append(removed, id)
		}
	}
	if len(removed) > 0 {
		m.config.InspectorPurge(removed)
	}
}

// activeRouteIDs returns the currently forwarding (active-desired) route IDs
// of an active generation. Unknown implementations yield nothing, never an
// over-broad purge.
func activeRouteIDs(active Active) []string {
	if active == nil {
		return nil
	}
	if lister, ok := active.(interface{ activeRouteIDs() []string }); ok {
		return lister.activeRouteIDs()
	}
	return nil
}
