package datacarrier

import (
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// supersededGrace bounds how long an endpoint admission a newer attachment of the same machine
// replaced still authorizes its old carrier. At a worker handover (pb update) the server names
// the new worker's carrier before that carrier has attached here; the old one keeps serving the
// route until the new one takes it (make-before-break), never longer than this.
const supersededGrace = 30 * time.Second

// supersededBy returns the endpoint and runtime admissions of prior (just replaced) and held
// (superseded earlier) that next replaces with a newer attachment of the same machine and route. An
// endpoint admission expires at most supersededGrace after it was first replaced. A runtime one
// stays up to runtimewire.DrainLimit: the old worker's carrier keeps serving the browser
// terminals it holds (it takes no new ones; the newer carrier is the route's current one), as nginx
// keeps an old worker's connections through a reload. Its own short expiry does not end it: the
// successor is the server's live word that this machine still holds the route. A route next drops,
// moves to another machine, or that an older generation names, is not held: that is a withdrawal,
// and it ends at once.
func supersededBy(prior, held, next map[string]ExpectedAdmission, now time.Time) map[string]ExpectedAdmission {
	successors := make(map[string]ExpectedAdmission, len(next))
	for _, admission := range next {
		successors[admission.RouteID] = admission
	}
	out := map[string]ExpectedAdmission{}
	keep := func(key string, old ExpectedAdmission, fresh bool) {
		successor, ok := successors[old.RouteID]
		if !ok || !old.Admitted || !(old.EndpointRoute() && successor.EndpointRoute() || old.runtimeRoute() && successor.runtimeRoute()) ||
			successor.Identity == old.Identity || successor.Identity.AccountID != old.Identity.AccountID ||
			successor.Identity.HostID != old.Identity.HostID || successor.Hostname != old.Hostname ||
			successor.AttachmentGeneration <= old.AttachmentGeneration {
			return
		}
		if fresh && old.runtimeRoute() {
			old.ExpiresAt = now.Add(runtimewire.DrainLimit)
		} else if fresh && old.ExpiresAt.After(now.Add(supersededGrace)) {
			old.ExpiresAt = now.Add(supersededGrace)
		}
		if now.Before(old.ExpiresAt) {
			out[key] = old
		}
	}
	for key, old := range held {
		keep(key, old, false)
	}
	for key, old := range prior {
		if current, ok := next[key]; ok && current.Identity == old.Identity {
			continue
		}
		// Keyed by session too: the newer admission shares the operation and route key.
		keep(key+"\x00"+old.Identity.SessionID, old, true)
	}
	return out
}

// Superseded reports whether identity's carrier may still serve routeID under a superseded
// admission (see supersededBy): its route stays attached until the newer carrier attaches it.
func (r *ExpectedAdmissionRegistry) Superseded(identity Identity, routeID string, now time.Time) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, admission := range r.supersededByIdentity[identity] {
		if admission.RouteID == routeID && now.Before(admission.ExpiresAt) {
			return true
		}
	}
	return false
}

// runtimeRoute reports whether the admission is an agent's runtime route (browser terminals ride it).
func (a ExpectedAdmission) runtimeRoute() bool {
	return a.Schema == runtimewire.Schema && a.Kind == "" && a.RouteKind == runtimewire.RouteKind
}
