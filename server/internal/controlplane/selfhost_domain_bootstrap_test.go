package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

func TestScopedTunnelDomainBootstrapOnPostgres(t *testing.T) {
	database, f := newTRK35PostgresFixture(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE paperboat.tunnel_routes SET match_type='managed' WHERE id=$1`, f.routeID)
	f.insertConnector(t, database, f.connectorOne, f.hostOne, f.sessionOne, 11)
	service := &EdgeService{store: database, clock: func() time.Time { return f.now }}
	if n, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || n != 1 {
		t.Fatalf("stage = %d, %v", n, err)
	}
	read := func() []dbsqlc.ListTunnelEdgeRouteAssignmentsForNodeV1Row {
		t.Helper()
		rows, err := service.ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, f.nodeID, f.epoch)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := read(); len(rows) != 1 || rows[0].ScopedDomainHostname != "" {
		t.Fatalf("managed projection = %+v", rows)
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET tls_spki_sha256=$2 WHERE id=$1`, f.nodeID, f.configHash)
	installation := "installation_" + f.suffix
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete)
 VALUES($1,$2,$3,$3,'ciphertext','domain bootstrap','tunnel','account',$4,$1,true)`, installation, f.nodeID, f.configHash, f.accountID)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_installations WHERE id=$1`, installation)
	})
	if rows := read(); len(rows) != 0 {
		t.Fatalf("scoped route without owned domain was admitted: %+v", rows)
	}
	domain := "domain_" + f.suffix
	hostname := "custom-" + f.suffix + ".example.test"
	exec(`INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state)
 VALUES($1,$2,$3,$4,$5,'exact','dns:test','pending','target.example.test','managed','pending','unknown','clear')`, domain, f.accountID, f.tunnelID, f.routeID, hostname)
	if rows := read(); len(rows) != 0 {
		t.Fatalf("unverified custom domain admitted: %+v", rows)
	}
	exec(`UPDATE paperboat.tunnel_domains SET ownership_state='verified' WHERE id=$1`, domain)
	rows := read()
	if len(rows) != 1 || rows[0].ScopedDomainHostname != hostname || rows[0].ScopedDomainMatchType != "exact" || rows[0].DomainBindings != "[]" {
		t.Fatalf("pre-TLS bootstrap = %+v", rows)
	}
	view := tunnelEdgeRouteJSON(rows[0])
	if view["match_hostname"] != hostname || view["match_type"] != "exact" || view["public_host"] != hostname || view["wildcard_suffix"] != nil {
		t.Fatalf("custom bootstrap projection = %#v", view)
	}
	exec(`UPDATE paperboat.tunnel_domains SET certificate_state='ready' WHERE id=$1`, domain)
	rows = read()
	var aliases []map[string]any
	if err := json.Unmarshal([]byte(rows[0].DomainBindings), &aliases); err != nil || len(aliases) != 1 || aliases[0]["hostname"] != hostname {
		t.Fatalf("ready alias = %s, %v", rows[0].DomainBindings, err)
	}
	exec(`UPDATE paperboat.selfhost_installations SET scope_kind='global' WHERE id=$1`, installation)
	if rows := read(); len(rows) != 1 || rows[0].ScopedDomainHostname != "" {
		t.Fatalf("global edge lost managed hostname: %+v", rows)
	}
	exec(`UPDATE paperboat.selfhost_installations SET scope_kind='account' WHERE id=$1`, installation)
	exec(`UPDATE paperboat.tunnel_domains SET hostname=$2,match_type='one_label_wildcard',certificate_state='pending' WHERE id=$1`, domain, "*.apps-"+f.suffix+".example.test")
	view = tunnelEdgeRouteJSON(read()[0])
	if view["match_type"] != "one_label_wildcard" || view["match_hostname"] != nil || view["wildcard_suffix"] != "apps-"+f.suffix+".example.test" {
		t.Fatalf("wildcard bootstrap = %#v", view)
	}
	exec(`UPDATE paperboat.tunnel_domains SET ownership_state='revoked' WHERE id=$1`, domain)
	if rows := read(); len(rows) != 0 {
		t.Fatalf("revoked custom domain retained admission: %+v", rows)
	}
}

func TestScopedDomainProjectionPreservesRouteAuthority(t *testing.T) {
	row := dbsqlc.ListTunnelEdgeRouteAssignmentsForNodeV1Row{RouteID: "route_1", AssignmentID: "assignment_1", AssignmentGeneration: 3, MatchType: "managed", MatchHostname: sql.NullString{String: "managed.example.test", Valid: true}, ScopedDomainHostname: "app.customer.test", ScopedDomainMatchType: "exact", AccessMode: "private", ViewerPolicyGeneration: 7}
	view := tunnelEdgeRouteJSON(row)
	if view["route_id"] != row.RouteID || view["assignment_id"] != row.AssignmentID || view["assignment_generation"] != row.AssignmentGeneration || view["access_mode"] != row.AccessMode || view["viewer_policy_generation"] != row.ViewerPolicyGeneration || view["match_hostname"] != row.ScopedDomainHostname {
		t.Fatalf("projection = %#v", view)
	}
}
