package testutil

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
)

// PublicationMachine supplies the parent host for fixtures that exercise
// publication state without enrolling or running a host. One account may have
// several publications on this same machine.
func PublicationMachine(t testing.TB, store *sql.DB, account string) string {
	t.Helper()
	id := "publication_host_" + account
	if _, err := store.ExecContext(t.Context(), `INSERT INTO paperboat.user_machines
 (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state)
 VALUES($1,$2,$1,'linux','amd64','/workspace','offline','released') ON CONFLICT(id) DO NOTHING`, id, account); err != nil {
		t.Fatal(err)
	}
	return id
}

// VerifiedWorkspaceDomain adds hostname as a verified domain of a workspace (team "" is the account's
// personal workspace), so tunnel and preview hostnames one level under it can be bound.
func VerifiedWorkspaceDomain(t testing.TB, store *sql.DB, account, team, hostname string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(hostname))
	id := "wsd_" + hex.EncodeToString(sum[:12])
	if _, err := store.ExecContext(t.Context(), `INSERT INTO paperboat.workspace_domains
 (id,account_id,owner_team_id,hostname,challenge_reference,ownership_state,verified_at,created_by_actor_id)
 VALUES($1,$2,NULLIF($3,''),$4,'account:'||$2,'verified',now(),$2) ON CONFLICT(hostname) DO NOTHING`, id, account, team, hostname); err != nil {
		t.Fatal(err)
	}
	return id
}

// ServeTunnelRoute makes one live managed edge serve a tunnel route on a live carrier of the tunnel's agent
// machine (paperboat.endpoint_route_ready holds for about an hour), for tests of what waits on every edge.
func ServeTunnelRoute(t testing.TB, store *sql.DB, account, machine, tunnel, route string) {
	t.Helper()
	node, epoch, session, carrier := "edge_serve_"+route, "epoch_serve_"+route, CarrierSessionID("serve:"+route), "crt_serve_"+route
	for _, step := range []struct {
		query string
		args  []any
	}{
		// A carrier attachment carries the machine's identity key; a publication fixture machine has none yet.
		{`UPDATE paperboat.user_machines SET public_identity_key='pk_test_'||md5(id)||md5(id) WHERE id=$1 AND public_identity_key IS NULL`, []any{machine}},
		{`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,registry_expires_at,region,failure_domain,transports,capacity_used,capacity_observed_at,tunnel_endpoints_epoch)
 VALUES($1,'default','v1',$2::text,'ready',true,now()+interval '1 hour',ARRAY['edge']::text[],now()+interval '2 hours','test','test',ARRAY['http2','http3']::text[],0,now(),$2::text)`, []any{node, epoch}},
		{`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) SELECT environment_id,environment_id,user_id,'active' FROM paperboat.user_machines WHERE id=$1 ON CONFLICT DO NOTHING`, []any{machine}},
		{`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) SELECT $1::text,environment_id,'runtime_https_wss',$1::text||'.runtime.example.test','127.0.0.1',38080 FROM paperboat.user_machines WHERE id=$2`, []any{carrier, machine}},
		{`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active,ready)
 SELECT $1,m.id,m.user_id,m.installation_generation,1,'process_serve',1,$2,$3,m.public_identity_key,1,$4::text,$4::text,'{}',now()+interval '1 hour',true,true FROM paperboat.user_machines m WHERE m.id=$5`, []any{carrier, node, epoch, session, machine}},
		{`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1::text,'tunnel',$2,$3,'public','persistent',$1::text) ON CONFLICT DO NOTHING`, []any{tunnel, account, machine}},
		{`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1::text,$2,$3,'http',$1::text) ON CONFLICT DO NOTHING`, []any{route, tunnel, strings.ToLower(strings.ReplaceAll(route, "_", "-")) + ".example.test"}},
		{`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,$4,$5,true,now()+interval '1 hour')`, []any{route, tunnel, node, epoch, session}},
	} {
		if _, err := store.ExecContext(t.Context(), step.query, step.args...); err != nil {
			t.Fatalf("serve tunnel route %s: %v", route, err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.ExecContext(context.Background(), `DELETE FROM paperboat.control_routes WHERE id=$1`, carrier)
		_, _ = store.ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
	})
}

// CarrierSessionID is a well-formed carrier session id (session_<uuid v4>) derived from seed, so fixtures that
// insert runtime_carrier_attachments satisfy its preview_session_id check.
func CarrierSessionID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:16])
	return "session_" + h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}
