package edgehttp

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A route has at most two carriers here: the current one and, for a moment after a worker
// handover (pb update), the standby the same machine's newer carrier replaced. Requests go to a
// live carrier whose host is not draining first; an idempotent request that fails on one carrier
// before any response is retried once on the other, the way net/http replays a GET on a broken
// reused connection (net/http/request.go isReplayable) and ngrok swaps a reconnected session in
// before the old one closes (ngrok-go internal/tunnel/client/reconnecting.go swapRaw).
// retryPreviewTransport (policy.go) separately retries a request once on the route as it then is.

// previewCandidate is one carrier of a route with the channel closed when it is withdrawn.
type previewCandidate struct {
	route   DataCarrierPreviewRoute
	revoked <-chan struct{}
}

// retireLocked ends the entry next replaces. An endpoint or runtime route the same machine's newer
// carrier took over becomes the standby, so its open streams (a held browser terminal, pb update)
// finish instead of being cut; anything else is withdrawn at once.
func (r *DataCarrierPreviewRegistry) retireLocked(old *dataCarrierPreviewRouteEntry, next DataCarrierPreviewRoute) {
	live := old.route.Server != nil
	if live {
		select {
		case <-old.route.Server.Done():
			live = false
		default:
		}
	}
	runtimeRoutes := old.route.Kind == runtimewire.RouteKind && next.Kind == runtimewire.RouteKind
	if !live || !(old.route.EndpointRoute && next.EndpointRoute || runtimeRoutes) || old.route.Identity.HostID != next.Identity.HostID || old.route.Server == next.Server {
		old.close()
		return
	}
	entries := append(r.standby[old.route.RouteID], old)
	if len(entries) > maxStandbys {
		entries[0].close()
		entries = entries[1:]
	}
	r.standby[old.route.RouteID] = entries
}

// maxStandbys bounds the replaced carriers a route keeps (each is a worker generation pb update replaced
// that still serves streams); the oldest closes first.
const maxStandbys = 8

// promoteStandbyLocked makes the newest live, not draining standby of the route its serving carrier, so the loss of the serving
// connection does not leave the route unserved until the next pull attaches it again.
func (r *DataCarrierPreviewRegistry) promoteStandbyLocked(routeID string) {
	standbys := r.standby[routeID]
	for i := len(standbys) - 1; i >= 0; i-- {
		next := standbys[i]
		if next.route.Server == nil {
			continue
		}
		select {
		case <-next.route.Server.Done():
			continue
		default:
		}
		if next.route.Server.DrainingNow() {
			continue // its host sent a goaway: the next pull attaches a carrier that is staying
		}
		r.standby[routeID] = append(standbys[:i:i], standbys[i+1:]...)
		if len(r.standby[routeID]) == 0 {
			delete(r.standby, routeID)
		}
		r.byRoute[routeID] = next
		r.byHost[next.host] = next
		r.activateDesiredAliasesForRouteLocked(routeID)
		return
	}
}

// dropStandbyLocked closes every standby of the route.
func (r *DataCarrierPreviewRegistry) dropStandbyLocked(routeID string) {
	for _, standby := range r.standby[routeID] {
		standby.close()
	}
	delete(r.standby, routeID)
}

// removeStandbyLocked closes and removes the route's standbys that match, and reports whether any did.
func (r *DataCarrierPreviewRegistry) removeStandbyLocked(routeID string, match func(*dataCarrierPreviewRouteEntry) bool) bool {
	kept, removed := r.standby[routeID][:0:0], false
	for _, standby := range r.standby[routeID] {
		if match(standby) {
			standby.close()
			removed = true
		} else {
			kept = append(kept, standby)
		}
	}
	if len(kept) == 0 {
		delete(r.standby, routeID)
	} else {
		r.standby[routeID] = kept
	}
	return removed
}

// candidates returns the live carriers of host's route, one whose host is not draining first.
func (r *DataCarrierPreviewRegistry) candidates(host string) []previewCandidate {
	current, revoked, ok := r.acquire(host)
	if !ok || current.Server == nil {
		return nil
	}
	out := []previewCandidate{{route: current, revoked: revoked}}
	r.mu.RLock()
	var standby *dataCarrierPreviewRouteEntry
	var route DataCarrierPreviewRoute
	if entries := r.standby[current.RouteID]; len(entries) != 0 {
		standby = entries[len(entries)-1] // the newest replaced carrier
		route = standby.route
	}
	r.mu.RUnlock()
	if standby == nil || route.Server == nil {
		return out
	}
	select {
	case <-route.Server.Done():
		return out
	default:
	}
	route.Hostname = current.Hostname
	out = append(out, previewCandidate{route: route, revoked: standby.done})
	if current.Server.DrainingNow() && !route.Server.DrainingNow() {
		out[0], out[1] = out[1], out[0]
	}
	return out
}

// roundTripCandidates sends request on the first candidate and retries once on the second when
// the first failed before the request was sent, or, for a replayable request, before any
// response arrived. A slow origin (response header timeout) is not retried, nor a browser or
// private request: its authority names one carrier session.
func (t *DataCarrierPreviewTransport) roundTripCandidates(request *http.Request, host string, candidates []previewCandidate) (*http.Response, error) {
	_, browser := request.Context().Value(browserDecisionKey{}).(connectorprotocol.IngressDecision)
	_, private := request.Context().Value(privateAccessRequestContextKey{}).(connectorprotocol.PrivateAccessRequest)
	if browser || private {
		candidates = candidates[:1]
	}
	for index, candidate := range candidates {
		last := index == len(candidates)-1
		response, sent, err := t.roundTripRoute(request, host, candidate.route, candidate.revoked, last)
		if err == nil || request.Context().Err() != nil {
			return response, err
		}
		var slow *dataCarrierPreviewResponseHeaderTimeoutError
		retry := !last && (!sent || retryablePreviewRequest(request)) && !errors.As(err, &slow)
		logPreviewRequestFailure(request, candidate.route, sent, retry, err)
		if !retry {
			return response, err
		}
	}
	return nil, ErrDataCarrierPreviewTransport
}

// logPreviewRequestFailure names the hop where one request failed: the carrier stream open, or
// forwarding over it, and the carrier's state. Bounded fields only; never headers or bodies.
func logPreviewRequestFailure(request *http.Request, route DataCarrierPreviewRoute, sent, retry bool, err error) {
	stage := "open"
	if sent {
		stage = "forward"
	}
	closed := false
	select {
	case <-route.Server.Done():
		closed = true
	default:
	}
	cause := err.Error()
	if len(cause) > 200 {
		cause = cause[:200]
	}
	slog.Warn("preview_carrier_request_failed", "route_id", route.RouteID, "session_id", route.Identity.SessionID,
		"process_generation", route.Identity.ProcessGeneration, "stage", stage, "method", request.Method,
		"carrier_closed", closed, "carrier_draining", route.Server.DrainingNow(), "retry", retry, "error", cause)
}
