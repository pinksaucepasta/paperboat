package tunnelv1

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/tunnelrouterules"
)

// A route's rules survive the database: stored with the route on create, read back, replaced and cleared by a patch (a
// same-rules patch changes nothing), and the table itself refuses rules on a route that is not http.
func TestRouteRewriteRulesRoundTripOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.accounts[0], "mch_trk07_primary_"+f.suffix
	tunnelID, routeID := "tun_rules_"+f.suffix, "rte_rules_"+f.suffix
	f.createTunnel(t, account, host, tunnelID, "rules-"+f.suffix)
	canonical, err := tunnelrouterules.Canonical(*sampleRules())
	if err != nil {
		t.Fatal(err)
	}
	route := trk07RouteInput(f, account, host, tunnelID, routeID, "api", "api-rules-"+f.suffix+".example.test")
	route.RewriteRules = canonical
	created, err := f.repository.CreateResourceRoute(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.repository.GetResourceRoute(ctx, account, tunnelID, routeID)
	if err != nil || !sameRewriteRules(stored.RewriteRules, canonical) {
		t.Fatalf("stored rules = %s (%v)", stored.RewriteRules, err)
	}
	// patching the identical rules is not a change
	patch := RouteRecord{RouteID: routeID, TunnelID: tunnelID, AccountID: account, RewriteRules: canonical, RewriteRulesSet: true,
		ExpectedGeneration: created.Route.Generation, OperationID: "op_p1_" + f.suffix, AuditEventID: "aud_p1_" + f.suffix, IdempotencyKey: "p1_" + f.suffix,
		RequestHash: route.RequestHash, ActorID: account, AuditActorID: account, ActorType: "user", RequestID: "req_p1_" + f.suffix, CorrelationID: "cor_p1_" + f.suffix, SourceMachineID: host, Now: f.now}
	same, err := f.repository.PatchResourceRoute(ctx, patch)
	if err != nil || same.Changed {
		t.Fatalf("the same rules counted as a change: %v changed=%v", err, same.Changed)
	}
	// clearing replaces them and bumps the generation
	patch.RewriteRules, patch.OperationID, patch.AuditEventID, patch.IdempotencyKey, patch.RequestID, patch.CorrelationID = []byte("{}"), "op_p2_"+f.suffix, "aud_p2_"+f.suffix, "p2_"+f.suffix, "req_p2_"+f.suffix, "cor_p2_"+f.suffix
	patch.RequestHash = [32]byte{2}
	cleared, err := f.repository.PatchResourceRoute(ctx, patch)
	if err != nil || !cleared.Changed || string(rewriteRulesOrEmpty(cleared.Route.RewriteRules)) == string(canonical) {
		t.Fatalf("clearing the rules: %v %+v", err, cleared)
	}
	// the table backstops the API: rules only on http routes
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_routes SET protocol='private_tcp', rewrite_rules=$2::jsonb WHERE id=$1`, routeID, string(canonical)); err == nil {
		t.Fatal("the database accepted rewrite rules on a non-http route")
	}
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_routes SET rewrite_rules='[]'::jsonb WHERE id=$1`, routeID); err == nil {
		t.Fatal("the database accepted rules that are not an object")
	}
}
