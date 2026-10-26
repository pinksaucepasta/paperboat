package runtimeattachment

import (
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

func TestOriginHealthReportsAcceptOnlyBoundedValidStates(t *testing.T) {
	now := time.Now().UTC()
	route := func(state, code string) runtimewire.OriginHealthRoute {
		return runtimewire.OriginHealthRoute{TunnelID: "tun_1", RouteID: "rte_1", State: state, Code: code}
	}
	for name, tc := range map[string]struct {
		routes []runtimewire.OriginHealthRoute
		want   bool
	}{
		"reachable":           {[]runtimewire.OriginHealthRoute{route("reachable", "")}, true},
		"unreachable coded":   {[]runtimewire.OriginHealthRoute{route("unreachable", "refused")}, true},
		"empty":               {nil, false},
		"reachable with code": {[]runtimewire.OriginHealthRoute{route("reachable", "refused")}, false},
		"unreachable no code": {[]runtimewire.OriginHealthRoute{route("unreachable", "")}, false},
		"unknown code":        {[]runtimewire.OriginHealthRoute{route("unreachable", "boom")}, false},
		"unknown state":       {[]runtimewire.OriginHealthRoute{route("ready", "")}, false},
		"no route id":         {[]runtimewire.OriginHealthRoute{{TunnelID: "tun_1", State: "reachable"}}, false},
		"too many":            {make([]runtimewire.OriginHealthRoute, runtimewire.MaximumOriginHealthRoutes+1), false},
	} {
		reports, ok := originHealthReports(runtimewire.OriginHealthRequest{Routes: tc.routes}, now)
		if ok != tc.want {
			t.Errorf("%s: accepted=%v, want %v", name, ok, tc.want)
		}
		if ok && reports[0].State != map[string]string{"reachable": "ready", "unreachable": "unreachable"}[tc.routes[0].State] {
			t.Errorf("%s: stored state %q", name, reports[0].State)
		}
	}
}
