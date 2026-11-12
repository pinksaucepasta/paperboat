package control

import (
	"context"
	"net/http"
	"testing"
)

// The edge reads a route's header, path and redirect rules from the server's desired-state answer: the client's own wire
// struct has to carry the field, or the rules never reach the edge (found on prod: the rules were stored and listed but
// the edge served every route as if it had none).
func TestHTTPClientCarriesARoutesRewriteRulesFromTheServer(t *testing.T) {
	client := controlClient(t, func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"complete":true,"routes":[{"route_id":"rte_1","route_revision":1,"kind":"runtime_https_wss","public_host":"app.example.test","target":{"host":"127.0.0.1","port":8080},`+
			`"protocol":"http","rewrite_rules":{"request_headers":[{"op":"set","name":"X-Env","value":"prod"}],"path_rewrite":{"from":"/api","to":"/v1"},"redirects":[{"from":"/old","code":308,"to":"/new"}]}}]}`), nil
	})
	snapshot, err := client.DesiredRouteSnapshot(context.Background(), "edge", "process_epoch")
	if err != nil {
		t.Fatal(err)
	}
	rules := snapshot.Routes[0].RewriteRules
	if len(snapshot.Routes) != 1 || rules == nil || len(rules.RequestHeaders) != 1 || rules.PathRewrite == nil || rules.PathRewrite.To != "/v1" || len(rules.Redirects) != 1 || rules.Redirects[0].Code != 308 {
		t.Fatalf("route rules after the control client = %+v", snapshot.Routes[0].RewriteRules)
	}
}
