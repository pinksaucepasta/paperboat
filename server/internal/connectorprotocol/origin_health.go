package connectorprotocol

import (
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// OriginHealthSchema identifies the host's origin reachability report for a tunnel connector.
const OriginHealthSchema = "paperboat.origin-health/v1"

const (
	// MaximumOriginHealthRoutes bounds one report; a tunnel has far fewer routes than this.
	MaximumOriginHealthRoutes = 64
	// OriginHealthMaximumAge is how old an observation may be when it arrives; older ones are stale.
	OriginHealthMaximumAge = 10 * time.Minute
	// OriginHealthClockSkew is how far into the future an observation may be.
	OriginHealthClockSkew = 2 * time.Minute
)

// OriginHealthRoute is the latest state of one route's local app. Codes are only set for
// unreachable: refused (nothing listens), timeout, unreachable (no route to host) or tls.
type OriginHealthRoute struct {
	RouteID    string    `json:"route_id"`
	State      string    `json:"state"`
	Code       string    `json:"code,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// OriginHealthRequest is the body of POST .../connectors/{connector_id}/origin-health. It binds the
// report to the exact active control session and configuration like carrier bootstrap does.
type OriginHealthRequest struct {
	Schema            string              `json:"schema"`
	SessionID         string              `json:"session_id"`
	ProcessGeneration uint64              `json:"process_generation"`
	ConfigGeneration  uint64              `json:"config_generation"`
	ConfigContentHash string              `json:"content_hash"`
	Routes            []OriginHealthRoute `json:"routes"`
}

func (r OriginHealthRequest) Validate(now time.Time) error {
	if r.Schema != OriginHealthSchema || wire.ValidateIdentifier(r.SessionID) != nil || r.ProcessGeneration == 0 || r.ConfigGeneration == 0 || !wire.ValidContentHash(r.ConfigContentHash) ||
		len(r.Routes) == 0 || len(r.Routes) > MaximumOriginHealthRoutes {
		return wire.ErrInvalidInput
	}
	seen := make(map[string]bool, len(r.Routes))
	for _, route := range r.Routes {
		if wire.ValidateIdentifier(route.RouteID) != nil || seen[route.RouteID] || route.ObservedAt.IsZero() ||
			route.ObservedAt.After(now.Add(OriginHealthClockSkew)) || route.ObservedAt.Before(now.Add(-OriginHealthMaximumAge)) {
			return wire.ErrInvalidInput
		}
		seen[route.RouteID] = true
		switch route.State {
		case "ready":
			if route.Code != "" {
				return wire.ErrInvalidInput
			}
		case "unreachable":
			if route.Code != "refused" && route.Code != "timeout" && route.Code != "unreachable" && route.Code != "tls" {
				return wire.ErrInvalidInput
			}
		default:
			return wire.ErrInvalidInput
		}
	}
	return nil
}
