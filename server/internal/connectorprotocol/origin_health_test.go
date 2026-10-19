package connectorprotocol

import (
	"strings"
	"testing"
	"time"
)

func validOriginHealth(now time.Time) OriginHealthRequest {
	return OriginHealthRequest{
		Schema: OriginHealthSchema, SessionID: "session_01", ProcessGeneration: 2, ConfigGeneration: 3,
		ConfigContentHash: "sha256:" + strings.Repeat("a", 64),
		Routes: []OriginHealthRoute{
			{RouteID: "route_a", State: "unreachable", Code: "refused", ObservedAt: now},
			{RouteID: "route_b", State: "ready", ObservedAt: now.Add(-time.Minute)},
		},
	}
}

func TestOriginHealthRequestValidation(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if err := validOriginHealth(now).Validate(now); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	for name, mutate := range map[string]func(*OriginHealthRequest){
		"wrong schema":        func(r *OriginHealthRequest) { r.Schema = "paperboat.origin-health/v2" },
		"no session":          func(r *OriginHealthRequest) { r.SessionID = "" },
		"no generation":       func(r *OriginHealthRequest) { r.ProcessGeneration = 0 },
		"bad hash":            func(r *OriginHealthRequest) { r.ConfigContentHash = "sha256:xyz" },
		"no routes":           func(r *OriginHealthRequest) { r.Routes = nil },
		"too many routes":     func(r *OriginHealthRequest) { r.Routes = make([]OriginHealthRoute, MaximumOriginHealthRoutes+1) },
		"duplicate route":     func(r *OriginHealthRequest) { r.Routes[1].RouteID = r.Routes[0].RouteID },
		"unknown state":       func(r *OriginHealthRequest) { r.Routes[0].State = "down" },
		"unreachable no code": func(r *OriginHealthRequest) { r.Routes[0].Code = "" },
		"unknown code":        func(r *OriginHealthRequest) { r.Routes[0].Code = "dns" },
		"ready with code":     func(r *OriginHealthRequest) { r.Routes[1].Code = "refused" },
		"observed in future":  func(r *OriginHealthRequest) { r.Routes[0].ObservedAt = now.Add(OriginHealthClockSkew + time.Second) },
		"observation too old": func(r *OriginHealthRequest) { r.Routes[1].ObservedAt = now.Add(-OriginHealthMaximumAge - time.Second) },
		"no observation time": func(r *OriginHealthRequest) { r.Routes[0].ObservedAt = time.Time{} },
	} {
		request := validOriginHealth(now)
		request.Routes = append([]OriginHealthRoute(nil), request.Routes...)
		mutate(&request)
		if request.Validate(now) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
