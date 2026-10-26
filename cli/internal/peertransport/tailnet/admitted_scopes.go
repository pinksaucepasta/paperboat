package tailnet

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// TrackAdmittedScope retains only an actually accepted native operation. Signed
// scope timestamps remain unchanged and cannot admit another operation.
func (a *Authority) TrackAdmittedScope(peerID, resourceID, capability, direction string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return nil, ErrAdmission
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID != peerID {
			continue
		}
		for _, scope := range peer.Scopes {
			if scope.ResourceID != resourceID || scope.Capability != capability || scope.Direction != direction || !scopeLive(scope, time.Now().Unix()) {
				continue
			}
			switch scope.ResourceKind {
			case "machine_access", "inspector", "config_compare":
				return a.retainScopeLocked(scope)
			}
		}
	}
	return nil, ErrAdmission
}

// TrackAdmittedCarrier retains the exact signed authority that admits a
// carrier. Personal-network authority takes precedence; resource-only peers
// retain their first eligible application authority until actual disconnect.
func (a *Authority) TrackAdmittedCarrier(peerID string) (func(), NetworkScope, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return nil, NetworkScope{}, ErrAdmission
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID != peerID {
			continue
		}
		var backing NetworkScope
		for _, scope := range peer.Scopes {
			if !scopeLive(scope, time.Now().Unix()) {
				continue
			}
			switch scope.ResourceKind {
			case "machine_network":
				if scope.Capability == "connect" {
					backing = scope
					break
				}
			case "machine_access", "inspector", "config_compare":
				if backing.ResourceID == "" {
					backing = scope
				}
			}
			if backing.ResourceKind == "machine_network" {
				break
			}
		}
		if backing.ResourceID == "" {
			return nil, NetworkScope{}, ErrAdmission
		}
		release, err := a.retainScopeLocked(backing)
		return release, backing, err
	}
	return nil, NetworkScope{}, ErrAdmission
}

func (a *Authority) retainScopeLocked(scope NetworkScope) (func(), error) {
	ref := api.AdmittedNetworkScope{ResourceKind: scope.ResourceKind, ResourceID: scope.ResourceID}
	if a.admitted[ref] == 0 && len(a.admitted) >= api.MaxAdmittedNetworkScopes {
		return nil, ErrAdmission
	}
	if a.admitted == nil {
		a.admitted = make(map[api.AdmittedNetworkScope]int)
	}
	a.admitted[ref]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.admitted[ref] <= 1 {
				delete(a.admitted, ref)
			} else {
				a.admitted[ref]--
			}
		})
	}, nil
}

// AllowsAdmittedCarrier checks current signed peer identity without reapplying
// snapshot or certificate credential expiry to an accepted carrier.
func (a *Authority) AllowsAdmittedCarrier(peerID string, binding NetworkBinding, backing NetworkScope) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.current == nil {
		return false
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID == peerID {
			if peer.Identity != binding {
				return false
			}
			for _, scope := range peer.Scopes {
				if scope.ResourceKind == backing.ResourceKind && scope.ResourceID == backing.ResourceID && scope.ResourceGeneration == backing.ResourceGeneration && scope.Capability == backing.Capability && scope.Direction == backing.Direction && scope.Port == backing.Port {
					return true
				}
			}
			return false
		}
	}
	return false
}

// AdmittedScopes is a bounded reference inventory, not cached credential
// history. Both caller and machine include it in their signed network refresh.
func (a *Authority) AdmittedScopes() []api.AdmittedNetworkScope {
	a.mu.Lock()
	defer a.mu.Unlock()
	refs := make([]api.AdmittedNetworkScope, 0, len(a.admitted))
	for ref := range a.admitted {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b api.AdmittedNetworkScope) int {
		if n := strings.Compare(a.ResourceKind, b.ResourceKind); n != 0 {
			return n
		}
		return strings.Compare(a.ResourceID, b.ResourceID)
	})
	return refs
}
