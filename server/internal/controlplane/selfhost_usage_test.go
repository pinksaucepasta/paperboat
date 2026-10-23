package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/selfhost"
)

type testBandwidthDebiter struct{ calls int }

func (d *testBandwidthDebiter) DebitEnvironmentBandwidthTx(context.Context, *db.Tx, string, int64, time.Time) (int64, bool, error) {
	d.calls++
	return 0, false, nil
}

func TestSelfhostUsageCannotChargeOrNameOtherResources(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := fmt.Sprint(now.UnixNano())
	f := newTunnelEdgeAcceptanceFixture("usage_"+suffix, now)
	installation := "usage_install_" + suffix
	environment := "env_" + f.hostOne
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, f.accountID)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.selfhost_installations WHERE id=$1`, installation)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1`, f.nodeID)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_environments WHERE id=$1`, environment)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
	})
	// The usage report is for a route the customer's node holds: stage the real
	// tunnel assignment as TestIndependentOperatorUsageIsBoundToItsAssignedRoutes does.
	f.insertBase(t, store)
	f.insertConnector(t, store, f.connectorOne, f.hostOne, f.sessionOne, 11)
	f.markConnectorPreReady(t, store, f.connectorOne, f.sessionOne)
	assignments := &EdgeService{store: store, clock: func() time.Time { return now }}
	if staged, err := assignments.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged == 0 {
		t.Fatalf("stage assignment = %d, %v", staged, err)
	}
	var generation int64
	var heldEnvironment string
	if err := store.SQL().QueryRowContext(ctx, `SELECT a.route_generation,m.environment_id FROM paperboat.tunnel_edge_route_assignments a JOIN paperboat.user_machines m ON m.id=a.host_id WHERE a.route_id=$1 AND a.edge_node_id=$2 LIMIT 1`, f.routeID, f.nodeID).Scan(&generation, &heldEnvironment); err != nil {
		t.Fatal(err)
	}
	if heldEnvironment != environment {
		t.Fatalf("assignment environment = %q, want %q", heldEnvironment, environment)
	}
	// A customer edge serves its account's self-hosted-only pool, and a route only
	// with a verified domain that names the edge's public ingress address (064).
	exec(`INSERT INTO paperboat.control_environments (id, workspace_id) VALUES ($1,$2)`, environment, "workspace_"+suffix)
	exec(`UPDATE paperboat.control_tunnel_nodes SET tls_spki_sha256=$2,public_ingress_ipv4='140.245.11.41',public_ingress_verified_at=now(),last_heartbeat_at=now() WHERE id=$1`, f.nodeID, make([]byte, 32))
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'ciphertext','usage test','tunnel','account',$4,$1,true)`, installation, f.nodeID, []byte(pub), f.accountID)
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2])`, f.accountID, installation)
	exec(`INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,expected_records,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,$4,$5,'exact','dns:usage-test','verified','target.example.test','[{"type":"A","value":"140.245.11.41"}]'::jsonb,'managed','ready','ready','clear')`, "dom_usage_"+suffix, f.accountID, f.tunnelID, f.routeID, "usage-"+suffix+".example.test")
	exec(`INSERT INTO paperboat.control_usage_verification_keys(key_id,edge_node_id,public_key,not_before,expires_at) VALUES($1,$2,$3,$4,$5)`, installation, f.nodeID, []byte(pub), now.Add(-time.Hour), now.Add(time.Hour))
	service := NewEdgeService(store, "managed")
	service.SetNodeCredentials(selfhost.New(store, "selfhost-test-encryption-key-32-bytes-long"))
	debiter := &testBandwidthDebiter{}
	service.SetBandwidthDebiter(debiter)
	request := edgeUsageRequest{OperationID: "usage_" + suffix, Node: f.nodeID, Epoch: f.epoch, Environment: environment, Route: f.routeID, Revision: generation, Direction: "egress", Bytes: 25, Start: now.Add(-time.Second), End: now}
	sign := func() {
		payload, _ := json.Marshal(signedUsageDocument{OperationID: request.OperationID, Key: signedUsageKey{Node: request.Node, Epoch: request.Epoch, Environment: request.Environment, Route: request.Route, Revision: request.Revision, Direction: request.Direction}, Bytes: request.Bytes, Start: request.Start, End: request.End})
		request.Payload, _ = json.Marshal(signedUsageEnvelope{Algorithm: "EdDSA", KeyID: installation, Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
	}
	sign()
	receipt, err := service.Usage(ctx, request)
	if err != nil || receipt.DeltaBytes != 25 {
		t.Fatalf("own usage: %v", err)
	}
	if debiter.calls != 0 {
		t.Fatal("operator report debited Paperboat bandwidth")
	}
	for _, scope := range []string{"team", "global", "account"} {
		scopeID := f.accountID
		if scope == "team" {
			scopeID = "usage_team_" + suffix
		}
		if scope == "global" {
			scopeID = "global"
		}
		if scope == "team" {
			exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, scopeID, f.accountID)
			exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, scopeID, f.accountID)
			t.Cleanup(func() {
				store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE team_id=$1`, scopeID)
				store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, scopeID)
			})
		}
		exec(`UPDATE paperboat.selfhost_installations SET scope_kind=$2,scope_id=$3 WHERE id=$1`, installation, scope, scopeID)
		exempt, err := service.authorizeSelfhostUsage(ctx, request)
		if err != nil || exempt != (scope != "global") {
			t.Fatalf("usage classification %s exempt=%v err=%v", scope, exempt, err)
		}
	}
	request.OperationID += "_other"
	request.Environment = "other_environment"
	sign()
	if _, err = service.Usage(ctx, request); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("other environment accepted: %v", err)
	}
	request.Environment = environment
	exec(`UPDATE paperboat.selfhost_installations SET state='removed',claim_complete=false WHERE id=$1`, installation)
	sign()
	if _, err = service.Usage(ctx, request); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("removed scoped installation usage: %v", err)
	}
}

// Paperboat's own pbh-installed (global) nodes are hosted edges: a report for a route that
// has moved to another edge is billed through the hosted path, not refused as an independent
// operator's (oracle retried one refused preview report ~240 times a minute).
func TestGlobalInstallationUsageIsHostedNotBoundToCurrentRoutes(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	suffix := fmt.Sprint(time.Now().UnixNano())
	seedUsageScope(t, store, suffix)
	node, installation := "node_"+suffix, "global_install_"+suffix
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'ciphertext','global usage test','tunnel','global','global',$1,true)`, installation, node, []byte(pub)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_installations WHERE id=$1`, installation)
	})
	service := NewEdgeService(store, "managed")
	service.SetNodeCredentials(selfhost.New(store, "selfhost-test-encryption-key-32-bytes-long"))
	request := edgeUsageRequest{Node: node, Environment: "env_" + suffix, Route: "preview_moved_" + suffix, Revision: 1}
	exempt, err := service.authorizeSelfhostUsage(ctx, request)
	if err != nil || exempt {
		t.Fatalf("global node report on a moved route: exempt=%v err=%v (want hosted, not refused)", exempt, err)
	}
}

// An independent operator's report for a route its node does not hold is refused (its
// signature proves origin only), even when the node does hold another of the account's routes.
func TestIndependentOperatorUsageIsBoundToItsAssignedRoutes(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newTunnelEdgeAcceptanceFixture(fmt.Sprintf("op_%d", now.UnixNano()), now)
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_installations WHERE node_id=$1`, f.nodeID)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, f.accountID)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID)
	})
	f.insertBase(t, store)
	f.insertConnector(t, store, f.connectorOne, f.hostOne, f.sessionOne, 11)
	f.markConnectorPreReady(t, store, f.connectorOne, f.sessionOne)
	service := &EdgeService{store: store, clock: func() time.Time { return now }}
	if staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10); err != nil || staged == 0 {
		t.Fatalf("stage assignment = %d, %v", staged, err)
	}
	var generation int64
	var environment string
	if err := store.SQL().QueryRowContext(ctx, `SELECT a.route_generation,m.environment_id FROM paperboat.tunnel_edge_route_assignments a JOIN paperboat.user_machines m ON m.id=a.host_id WHERE a.route_id=$1 AND a.edge_node_id=$2 LIMIT 1`, f.routeID, f.nodeID).Scan(&generation, &environment); err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	installation := "op_install_" + f.suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'ciphertext','operator usage test','tunnel','account',$4,$1,true)`, installation, f.nodeID, []byte(pub), f.accountID); err != nil {
		t.Fatal(err)
	}
	service.SetNodeCredentials(selfhost.New(store, "selfhost-test-encryption-key-32-bytes-long"))
	held := edgeUsageRequest{Node: f.nodeID, Environment: environment, Route: f.routeID, Revision: generation}
	other := held
	other.Route = "route_not_held_" + f.suffix
	if _, err := service.authorizeSelfhostUsage(ctx, other); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("a route the operator's node does not hold was accepted: %v", err)
	}
}
