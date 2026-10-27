package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type tunnelEdgeAcceptanceFixture struct {
	suffix, accountID, tunnelID, routeID, nodeID, epoch string
	hostOne, hostTwo                                    string
	sessionOne, sessionTwo                              string
	publicKeyOne, publicKeyTwo                          string
	thumbprintOne, thumbprintTwo                        string
	configHash                                          []byte
	now                                                 time.Time
}

func newTunnelEdgeAcceptanceFixture(suffix string, now time.Time) tunnelEdgeAcceptanceFixture {
	publicKey := func(label string) (string, string) {
		seed := sha256.Sum256([]byte("trk15-machine:" + label + ":" + suffix))
		private := ed25519.NewKeyFromSeed(seed[:])
		public := private.Public().(ed25519.PublicKey)
		thumbprint, err := connectorprotocol.IdentityThumbprint(public)
		if err != nil {
			panic(err)
		}
		return base64.RawURLEncoding.EncodeToString(public), thumbprint
	}
	publicKeyOne, thumbprintOne := publicKey("one")
	publicKeyTwo, thumbprintTwo := publicKey("two")
	configHash := sha256.Sum256([]byte("trk15-config:" + suffix))
	return tunnelEdgeAcceptanceFixture{
		suffix: suffix, accountID: "usr_trk15_" + suffix, tunnelID: "tun_trk15_" + suffix,
		routeID: "rte_trk15_" + suffix, nodeID: "edge-trk15-" + suffix, epoch: "edge-process-trk15-" + suffix,
		hostOne: "mch_trk15_a_" + suffix, hostTwo: "mch_trk15_b_" + suffix,
		sessionOne: "ses_trk15_a_" + suffix, sessionTwo: "ses_trk15_b_" + suffix,
		publicKeyOne: publicKeyOne, publicKeyTwo: publicKeyTwo,
		thumbprintOne: thumbprintOne, thumbprintTwo: thumbprintTwo,
		configHash: configHash[:], now: now,
	}
}

func (f tunnelEdgeAcceptanceFixture) insertBase(t *testing.T, database *db.DB) {
	t.Helper()
	ctx := context.Background()
	endpointID := testutil.EndpointID("tunnel-edge:" + f.suffix)
	endpoint := "https://" + testutil.EndpointLabel(endpointID) + ".tunnels.example.test"
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, []any{f.accountID, "workos_" + f.suffix, f.accountID + "@example.test"}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,public_identity_key) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',$4),($5,$2,$6,'linux','amd64','/workspace','online','occupied',$7)`, []any{f.hostOne, f.accountID, "env_" + f.hostOne, f.publicKeyOne, f.hostTwo, "env_" + f.hostTwo, f.publicKeyTwo}},
		{`INSERT INTO paperboat.tunnels (id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at) VALUES ($1,$2,$3,'active','public',1,$4,$5,$6,$2,'pending',$7,$7)`, []any{f.tunnelID, f.accountID, "trk15-" + f.suffix, endpointID, endpoint, f.hostOne, f.now}},
		{`INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES ($1,$2,'web','http','exact',$3,'/','http','127.0.0.1:3000',$4,$4,$5,$5)`, []any{f.routeID, f.tunnelID, "trk15-" + f.suffix + ".example.test", f.accountID, f.now}},
		{`INSERT INTO paperboat.tunnel_config_generations (tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,created_at,activated_at,retained_until) VALUES ($1,1,$2,convert_to(jsonb_build_object('routes',jsonb_build_array(jsonb_build_object('id',$6::text)))::text,'UTF8'),'active',$3,$4,$4,$5)`, []any{f.tunnelID, f.configHash, f.accountID, f.now, f.now.Add(time.Hour), f.routeID}},
		{`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES ($1,'trk15-zone-a','1.0',$2,'ready',true,$3,'region-a','domain-a',ARRAY['edge'],ARRAY['http3','http2'],100,0,$3,$3::timestamptz+interval '1 hour')`, []any{f.nodeID, f.epoch, f.now}},
	}
	for _, statement := range statements {
		if _, err := database.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func safeTunnelEdgeAcceptanceDSN(dsn string) bool {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	return strings.HasSuffix(name, "_test") && name != "_test"
}

// An HTTP catch-all route has no hostname of its own; the edge refuses a catch-all with an empty
// public host (route_invalid), which failed every route reconcile on the edge and left a tunnel
// with a second route pending forever. The projection gives it the tunnel's endpoint host.
func TestCatchAllRouteIsProjectedOnTheTunnelEndpointHostOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the PostgreSQL acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil || !safeTunnelEdgeAcceptanceDSN(dsn) {
		t.Fatalf("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database: %v", err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, database); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("%d", time.Now().UnixNano()), now)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, fixture.accountID)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fixture.nodeID)
		_ = database.Close()
	})
	fixture.insertBase(t, database)
	fixture.insertEngine(t, database)
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnels SET access_mode='private' WHERE id=$1`, fixture.tunnelID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_routes SET match_type='catch_all', match_hostname=NULL, path_prefix='/two' WHERE id=$1`, fixture.routeID); err != nil {
		t.Fatal(err)
	}
	service := &EdgeService{store: database, clock: func() time.Time { return now }}
	rows, err := service.ListTunnelEdgeRouteAssignmentsForNodeV1(ctx, fixture.nodeID, fixture.epoch)
	if err != nil || len(rows) != 1 {
		t.Fatalf("edge snapshot = %d, %v", len(rows), err)
	}
	var endpoint string
	if err := database.SQL().QueryRowContext(ctx, `SELECT stable_endpoint FROM paperboat.tunnels WHERE id=$1`, fixture.tunnelID).Scan(&endpoint); err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimPrefix(endpoint, "https://"); rows[0].PublicHost != want || rows[0].MatchType != "catch_all" || rows[0].MatchHostname.Valid {
		t.Fatalf("catch-all projected as host=%q match=%q hostname=%v, want host %q", rows[0].PublicHost, rows[0].MatchType, rows[0].MatchHostname, want)
	}
}

// insertEngine serves the fixture tunnel's route on hostOne's agent: a runtime carrier to the fixture
// edge and the endpoint's assignment on it, not yet observed by the edge (as the reconciler leaves it).
func (f tunnelEdgeAcceptanceFixture) insertEngine(t *testing.T, database *db.DB) {
	t.Helper()
	carrierRoute := "runtime_" + f.suffix
	admission := fmt.Sprintf(`{"binding":{"tunnel_id":"carrier_%s","connector_id":"carrier_connector_%s","config_generation":1},"config_content_hash":"sha256:%x"}`, f.suffix, f.suffix, f.configHash)
	host := "t-" + strings.ReplaceAll(f.suffix, "_", "-") + ".tunnels.example.test"
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE paperboat.control_tunnel_nodes SET endpoints_epoch=process_epoch,tunnel_endpoints_epoch=process_epoch WHERE id=$1`, []any{f.nodeID}},
		{`UPDATE paperboat.user_machines SET worker_generation=1 WHERE id=$1`, []any{f.hostOne}},
		{`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) SELECT environment_id,environment_id,user_id,'active' FROM paperboat.user_machines WHERE id=$1 ON CONFLICT DO NOTHING`, []any{f.hostOne}},
		{`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) SELECT $1,environment_id,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080 FROM paperboat.user_machines WHERE id=$2`, []any{carrierRoute, f.hostOne}},
		{`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active,agent_capabilities) SELECT $1,m.id,m.user_id,m.installation_generation,1,'process_engine',1,$2,$3,m.public_identity_key,1,$4,$4,$5::jsonb,now()+interval '1 hour',true,now(),true,ARRAY['endpoints','tunnel_endpoints'] FROM paperboat.user_machines m WHERE m.id=$6`, []any{carrierRoute, f.nodeID, f.epoch, f.carrierSession(), admission, f.hostOne}},
		{`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) SELECT id,'tunnel',account_id,created_by_host_id,access_mode,'persistent',id FROM paperboat.tunnels WHERE id=$1`, []any{f.tunnelID}},
		{`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1,$2,$3,'http',$1)`, []any{f.routeID, f.tunnelID, host}},
		{`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id) VALUES($1,$2,$3,$4,$5)`, []any{f.routeID, f.tunnelID, f.nodeID, f.epoch, f.carrierSession()}},
	} {
		if _, err := database.SQL().ExecContext(context.Background(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_routes WHERE id=$1`, carrierRoute)
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.endpoints WHERE id=$1`, f.tunnelID)
	})
}

// carrierSession is the fixture's runtime carrier session id, in the id format carriers must use.
func (f tunnelEdgeAcceptanceFixture) carrierSession() string {
	return fmt.Sprintf("session_00000000-0000-4000-8000-%012d", f.now.UnixNano()%1e12)
}

// observation is the edge's report of the fixture route by the engine's assignment id, carrying the
// carrier identity the edge admitted.
func (f tunnelEdgeAcceptanceFixture) observation(node, epoch, session, state string) RouteObservation {
	return RouteObservation{RouteID: f.routeID, AssignmentID: "epa_" + f.routeID, AssignmentGeneration: 1, RouteRevision: 1, EdgeNodeID: node, EdgeProcessEpoch: epoch,
		ConnectorID: "carrier_connector_" + f.suffix, HostID: f.hostOne, ConnectorGeneration: 1, ConnectorSessionID: session, ConnectorProcessGeneration: 1, ConfigGeneration: 1,
		ConfigContentHash: fmt.Sprintf("sha256:%x", f.configHash), AccessMode: "private", ViewerPolicyGeneration: 1, State: state}
}

// setCarrierHost points the fixture's runtime carrier at a Paperboat-managed host or, for a self-hosted
// pool, at one of the account's own: the carrier is valid only on the host its pool mode names.
func (f tunnelEdgeAcceptanceFixture) setCarrierHost(t *testing.T, database *db.DB, selfHosted bool) {
	t.Helper()
	host := "managed_public_host"
	if selfHosted {
		host = "'self-' || id || '.runtime.example.test'"
	}
	if _, err := database.SQL().ExecContext(context.Background(), `UPDATE paperboat.control_routes SET public_host=`+host+` WHERE id=$1`, "runtime_"+f.suffix); err != nil {
		t.Fatal(err)
	}
}
