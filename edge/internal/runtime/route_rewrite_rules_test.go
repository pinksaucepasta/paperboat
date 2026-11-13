package runtime

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
)

// The rules an assignment carries reach the matcher for http routes only, validated again; a document that no longer
// validates is dropped (the route is served without rules) instead of failing the snapshot.
func TestEdgeRewriteRulesAreForHTTPRoutesAndRevalidated(t *testing.T) {
	good := &tunnelrouterules.Rules{RequestHeaders: []tunnelrouterules.HeaderRule{{Op: "SET", Name: "x-a", Value: "1"}}}
	if got := edgeRewriteRules(control.RouteAssignment{Protocol: "http", RewriteRules: good}); got == nil || got.RequestHeaders[0].Name != "X-A" {
		t.Fatalf("http rules = %+v", got)
	}
	if edgeRewriteRules(control.RouteAssignment{Protocol: "tcp", RewriteRules: good}) != nil {
		t.Fatal("a tcp route carries rules")
	}
	if edgeRewriteRules(control.RouteAssignment{Protocol: "http"}) != nil || edgeRewriteRules(control.RouteAssignment{Protocol: "http", RewriteRules: &tunnelrouterules.Rules{}}) != nil {
		t.Fatal("no rules became rules")
	}
	bad := &tunnelrouterules.Rules{RequestHeaders: []tunnelrouterules.HeaderRule{{Op: "set", Name: "X-Forwarded-For", Value: "1.2.3.4"}}}
	if edgeRewriteRules(control.RouteAssignment{Protocol: "http", RewriteRules: bad}) != nil {
		t.Fatal("rules that fail validation were applied")
	}
}
