package tunnelv1

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
)

func sampleRules() *tunnelrouterules.Rules {
	return &tunnelrouterules.Rules{
		RequestHeaders: []tunnelrouterules.HeaderRule{{Op: "SET", Name: "x-env", Value: "prod"}},
		PathRewrite:    &tunnelrouterules.PathRewrite{From: "/api/", To: "/"},
		Redirects:      []tunnelrouterules.Redirect{{From: "/old", Code: 308, To: "/new"}},
	}
}

// A route's rules are validated and stored in canonical form on create and patch, for http routes only; the stored JSON
// comes back in the view.
func TestRouteRewriteRulesAreNormalizedOnCreateAndPatchForHTTPRoutesOnly(t *testing.T) {
	repo := &fakeResourceRepository{}
	service := resourceServiceForTest(t, repo)
	if _, err := service.CreateRoute(context.Background(), testRequest(false), "tun_1", RouteCreateRequest{
		Name: "web", Protocol: "http", MatchType: "catch_all", Origin: RouteOriginRequest{Scheme: "http", Address: "127.0.0.1:3000"},
		RewriteRules: sampleRules(), Mutation: ResourceMutationInput{IdempotencyKey: "rules_1", RequestHash: testHash()},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := tunnelrouterules.Decode(repo.createRoute.RewriteRules)
	if err != nil || stored.RequestHeaders[0].Op != "set" || stored.RequestHeaders[0].Name != "X-Env" || stored.PathRewrite.From != "/api" {
		t.Fatalf("stored rules = %s (%v)", repo.createRoute.RewriteRules, err)
	}
	for name, input := range map[string]RouteCreateRequest{
		"a tcp route": {Name: "db", Protocol: "tcp_private", MatchType: "catch_all", Origin: RouteOriginRequest{Scheme: "tcp", Address: "127.0.0.1:5432"}, RewriteRules: sampleRules()},
		"an edge header": {Name: "web", Protocol: "http", MatchType: "catch_all", Origin: RouteOriginRequest{Scheme: "http", Address: "127.0.0.1:3000"},
			RewriteRules: &tunnelrouterules.Rules{RequestHeaders: []tunnelrouterules.HeaderRule{{Op: "set", Name: "X-Forwarded-For", Value: "1.2.3.4"}}}},
	} {
		input.Mutation = ResourceMutationInput{IdempotencyKey: "rules_bad", RequestHash: testHash()}
		if _, err := service.CreateRoute(context.Background(), testRequest(false), "tun_1", input); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
	// a patch replaces the rules; an empty set clears them
	if _, err := service.PatchRoute(context.Background(), testRequest(false), "tun_1", "rte_1", RoutePatchRequest{
		RewriteRules: sampleRules(), RewriteRulesSet: true, Mutation: ResourceMutationInput{ExpectedGeneration: 1, IdempotencyKey: "rules_2", RequestHash: testHash()},
	}); err != nil || !repo.patchRoute.RewriteRulesSet || len(repo.patchRoute.RewriteRules) < 3 {
		t.Fatalf("patch with rules: %v %+v", err, repo.patchRoute)
	}
	if _, err := service.PatchRoute(context.Background(), testRequest(false), "tun_1", "rte_1", RoutePatchRequest{
		RewriteRulesSet: true, Mutation: ResourceMutationInput{ExpectedGeneration: 2, IdempotencyKey: "rules_3", RequestHash: testHash()},
	}); err != nil || !repo.patchRoute.RewriteRulesSet || string(repo.patchRoute.RewriteRules) != "{}" {
		t.Fatalf("patch that clears: %v rules=%s set=%v", err, repo.patchRoute.RewriteRules, repo.patchRoute.RewriteRulesSet)
	}
	row := resourceRoute("rte_1", "http", "managed")
	row.RewriteRules = []byte(`{"redirects": [{"to": "/new", "code": 308, "from": "/old"}]}`) // as PostgreSQL prints it
	if view := routeView(row); view.RewriteRules == nil || view.RewriteRules.Redirects[0].To != "/new" {
		t.Fatalf("view rules = %+v", view.RewriteRules)
	}
	if view := routeView(resourceRoute("rte_2", "http", "managed")); view.RewriteRules != nil {
		t.Fatalf("a route with no rules shows %+v", view.RewriteRules)
	}
	var none dbsqlc.TunnelRoute
	none.RewriteRules = []byte(`{}`)
	if routeView(none).RewriteRules != nil {
		t.Fatal("empty rules are not omitted")
	}
}

// PostgreSQL prints jsonb with spaces and its own key order; the API stores compact JSON. A patch that sends the same
// rules must not count as a change (it would bump the route generation for nothing).
func TestSameRewriteRulesComparesByMeaning(t *testing.T) {
	if !sameRewriteRules([]byte(`{"redirects": [{"to": "/new", "code": 308, "from": "/old"}]}`), []byte(`{"redirects":[{"from":"/old","code":308,"to":"/new"}]}`)) {
		t.Fatal("the same rules in two spellings differ")
	}
	if !sameRewriteRules(nil, []byte(`{}`)) {
		t.Fatal("no rules and an empty document differ")
	}
	if sameRewriteRules([]byte(`{"redirects":[{"from":"/old","code":308,"to":"/new"}]}`), []byte(`{"redirects":[{"from":"/old","code":301,"to":"/new"}]}`)) {
		t.Fatal("different rules are the same")
	}
	candidate := dbsqlc.TunnelRoute{Protocol: "tcp", RewriteRules: []byte(`{"redirects":[{"from":"/a","code":301,"to":"/b"}]}`)}
	if err := validateEffectiveRouteCandidate(dbsqlc.Tunnel{}, candidate); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a tcp route with rules: %v", err)
	}
}
