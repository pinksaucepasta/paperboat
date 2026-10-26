package tailnet

import (
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// withdrawalGrace bounds how long a withdrawn peer keeps its packet path (F5-10: close, then cut). The newer
// configuration is already in force, so nothing new is admitted for it; the path only carries the closes of its
// existing sessions and streams, which otherwise could not reach it and left the peer waiting on an idle timeout.
var withdrawalGrace = 2 * time.Second

type withdrawnPeer struct {
	peer  NetworkPeer
	until time.Time
}

// withdrawLocked records the peers of previous that next no longer lists. Those holding admitted scopes keep
// their path until they release them or withdrawalGrace passes; the rest are cut at once.
func (a *Authority) withdrawLocked(previous *NetworkConfiguration, next NetworkConfiguration) {
	if previous == nil || previous.Self != next.Self {
		return
	}
	listed := make(map[string]bool, len(next.Peers))
	for _, p := range next.Peers {
		listed[p.Identity.EndpointID] = true
	}
	until := time.Now().Add(withdrawalGrace)
	for _, p := range previous.Peers {
		if listed[p.Identity.EndpointID] || !a.holdsScopesLocked(p) {
			continue
		}
		if a.withdrawing == nil {
			a.withdrawing = make(map[string]withdrawnPeer)
		}
		a.withdrawing[p.Identity.EndpointID] = withdrawnPeer{peer: p, until: until}
		time.AfterFunc(withdrawalGrace, a.cutWithdrawn)
	}
	for id := range a.withdrawing {
		if listed[id] {
			delete(a.withdrawing, id) // listed again: its own entry in the configuration now applies
		}
	}
}

// installedPeersLocked is the configuration's peers plus those still within their withdrawal grace.
func (a *Authority) installedPeersLocked() []NetworkPeer {
	if len(a.withdrawing) == 0 {
		return a.current.Peers
	}
	peers := append([]NetworkPeer(nil), a.current.Peers...)
	for _, w := range a.withdrawing {
		peers = append(peers, w.peer)
	}
	return peers
}

// holdsScopesLocked reports whether a session or stream admitted under one of p's scopes is still open.
// shortcut: admitted scopes are counted per resource, not per peer, so a resource shared with another open peer
// keeps the path until withdrawalGrace; switch to per-peer counts if that delay ever matters.
func (a *Authority) holdsScopesLocked(p NetworkPeer) bool {
	for _, scope := range p.Scopes {
		if a.admitted[api.AdmittedNetworkScope{ResourceKind: scope.ResourceKind, ResourceID: scope.ResourceID}] > 0 {
			return true
		}
	}
	return false
}

// cutWithdrawnLocked drops the paths of withdrawn peers whose sessions closed or whose grace passed.
func (a *Authority) cutWithdrawnLocked() {
	now := time.Now()
	cut := false
	for id, w := range a.withdrawing {
		if !now.Before(w.until) || !a.holdsScopesLocked(w.peer) {
			delete(a.withdrawing, id)
			cut = true
		}
	}
	if cut && a.current != nil && !a.closed {
		_ = a.replaceLocked()
	}
}

func (a *Authority) cutWithdrawn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cutWithdrawnLocked()
}
