package selfhost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

func startTeamEnrollmentHelper(t *testing.T, binary, capability string) (string, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	command := exec.Command(binary, "install", "--no-service", "--state-dir", directory, "--endpoint-host", "140.245.11.41", "--claim-listen", address, "--enable", capability)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install packaged helper: %v: %s", err, output)
	}
	output, err := exec.Command(binary, "selfhost", "code", "--state-dir", directory).Output()
	if err != nil {
		t.Fatal(err)
	}
	var code string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "Self-host code: ") {
			code = strings.TrimPrefix(line, "Self-host code: ")
		}
	}
	if code == "" {
		t.Fatal("helper omitted enrollment code")
	}
	command = exec.Command(binary, "serve", "--state-dir", directory, "--binary-dir", filepath.Dir(binary))
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper listener unavailable")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return address, code
}

func TestPackagedHelperTeamEnrollmentAndPlacementOnPostgres(t *testing.T) {
	dsn, binary := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"), os.Getenv("PBH_TEST_BINARY")
	if dsn == "" || binary == "" {
		t.Skip("requires isolated PostgreSQL and packaged pbh")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := t.Context()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := randomIdentity()
	owner, admin, member, outsider := "team_owner_"+suffix, "team_admin_"+suffix, "team_member_"+suffix, "team_outsider_"+suffix
	team := "enrollment_team_" + suffix
	sqlExec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{owner, admin, member, outsider} {
		sqlExec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		rows, err := store.SQL().QueryContext(cleanup, `SELECT id,node_id FROM paperboat.selfhost_installations WHERE scope_kind='team' AND scope_id=$1`, team)
		if err != nil {
			t.Errorf("fixture cleanup: %v", err)
		} else {
			type item struct{ id, node string }
			var items []item
			for rows.Next() {
				var item item
				if err := rows.Scan(&item.id, &item.node); err != nil {
					t.Errorf("fixture cleanup: %v", err)
				} else {
					items = append(items, item)
				}
			}
			if err := rows.Err(); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
			rows.Close()
			for _, item := range items {
				for _, statement := range []struct {
					q  string
					id string
				}{{`DELETE FROM paperboat.selfhost_placements WHERE installation_id=$1`, item.id}, {`DELETE FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1`, item.node}, {`DELETE FROM paperboat.selfhost_installations WHERE id=$1`, item.id}, {`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, item.node}} {
					if _, err := store.SQL().ExecContext(cleanup, statement.q, statement.id); err != nil {
						t.Errorf("fixture cleanup: %v", err)
					}
				}
			}
		}
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.selfhost_pool_policies WHERE (scope_kind='team' AND scope_id=$1) OR (scope_kind='account' AND scope_id IN($2,$3,$4,$5))`, team, owner, admin, member, outsider)
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id IN($1,$2,$3,$4)`, owner, admin, member, outsider)
	})
	sqlExec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	sqlExec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true),($1,$4,1,'member',true)`, team, owner, admin, member)
	service := New(store, "selfhost-team-test-encryption-key-32-bytes-long")
	service.ControlURL = "https://control.example.test"
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := signer.JWKSJSON()
	if err != nil {
		t.Fatal(err)
	}
	service.TrustDocuments = func(context.Context) ([]byte, []byte, error) {
		return jwks, json.RawMessage(`{"version":1,"revocations":[]}`), nil
	}
	scope := Scope{Kind: "team", ID: team}
	address, code := startTeamEnrollmentHelper(t, binary, "both")
	var dials atomic.Int64
	service.DialAddress = func(context.Context, string) (string, error) { dials.Add(1); return address, nil }
	request := EnrollRequest{Address: address, Code: code, Scope: scope}
	for _, actor := range []string{member, outsider} {
		if _, err = service.Enroll(ctx, actor, request, false); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("unauthorized team enrollment by %s: %v", actor, err)
		}
	}
	global := request
	global.Scope = Scope{Kind: "global"}
	if _, err = service.Enroll(ctx, owner, global, false); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("customer global enrollment: %v", err)
	}
	if dials.Load() != 0 {
		t.Fatal("unauthorized enrollment contacted helper")
	}
	ownerInstalls, err := service.Enroll(ctx, owner, request, false)
	if err != nil || len(ownerInstalls) != 2 {
		t.Fatalf("owner enrollment: %v", err)
	}
	for _, installation := range ownerInstalls {
		var epoch, state string
		var ready bool
		var generation int64
		if err = store.SQL().QueryRowContext(ctx, `SELECT process_epoch,state,ready,node_generation FROM paperboat.control_tunnel_nodes WHERE id=$1`, installation.NodeID).Scan(&epoch, &state, &ready, &generation); err != nil {
			t.Fatal(err)
		}
		if epoch != "" || state != "registered" || ready || generation != 1 {
			t.Fatalf("enrolled runtime state epoch=%q state=%s ready=%t generation=%d", epoch, state, ready, generation)
		}
	}
	adminAddress, adminCode := startTeamEnrollmentHelper(t, binary, "relay")
	service.DialAddress = func(context.Context, string) (string, error) { dials.Add(1); return adminAddress, nil }
	adminInstalls, err := service.Enroll(ctx, admin, EnrollRequest{Address: adminAddress, Code: adminCode, Scope: scope}, false)
	if err != nil || len(adminInstalls) != 3 {
		t.Fatalf("admin enrollment: %v", err)
	}
	installations := adminInstalls
	for _, installation := range installations {
		if installation.ScopeKind != "team" || installation.ScopeID != team {
			t.Fatal("claim escaped team scope")
		}
		for _, actor := range []string{owner, admin, member, outsider} {
			var eligible bool
			if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.node_pool_eligible($1,$2,$3)`, installation.NodeID, actor, installation.Capability).Scan(&eligible); err != nil || eligible != (actor != outsider) {
				t.Fatalf("team eligibility actor=%s eligible=%v: %v", actor, eligible, err)
			}
		}
	}
	for _, actor := range []string{owner, admin, member, outsider} {
		listed, err := service.List(ctx, actor)
		expected := len(installations)
		if actor == outsider {
			expected = 0
		}
		if err != nil || len(listed) != expected {
			t.Fatalf("team list count %d want %d: %v", len(listed), expected, err)
		}
	}
	relayIDs := []string{}
	var tunnel Installation
	for _, installation := range installations {
		if installation.Capability == "relay" {
			relayIDs = append(relayIDs, installation.InstallationID)
		} else {
			tunnel = installation
		}
	}
	policy := Policy{Mode: "self-hosted-only", InstallationIDs: relayIDs}
	for _, actor := range []string{owner, admin, member} {
		expected := ErrInvalid
		if actor == member {
			expected = ErrUnauthorized
		}
		if _, err = service.SetPoolScoped(ctx, actor, "relay", policy, scope); !errors.Is(err, expected) {
			t.Fatalf("team pool write actor=%s: %v", actor, err)
		}
		if _, err = service.PoolScoped(ctx, actor, "relay", scope); !errors.Is(err, ErrInvalid) {
			t.Fatalf("team pool read actor=%s: %v", actor, err)
		}
	}
	if _, err = service.SetPool(ctx, admin, "relay", policy); err != nil {
		t.Fatalf("personal pool selecting visible team nodes: %v", err)
	}
	managedRelay := "team_pool_managed_" + suffix
	sqlExec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'managed','1.0','managed_fixture','managed.example.test',443,444,'ready',true,'test','test-managed',ARRAY['relay'],ARRAY['derp_quic','derp_wss'],256,0,now(),now()+interval '1 hour')`, managedRelay)
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, managedRelay)
	})
	assertManagedRelay := func(want bool) {
		t.Helper()
		var eligible bool
		if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.node_pool_eligible($1,$2,'relay')`, managedRelay, admin).Scan(&eligible); err != nil || eligible != want {
			t.Fatalf("managed relay eligibility=%v want %v: %v", eligible, want, err)
		}
	}
	assertManagedRelay(false)
	// This is a committed membership revocation, consumed directly by every scope
	// query. Team lifecycle tests separately exercise its mutation endpoint.
	sqlExec(`UPDATE paperboat.team_members SET active=false,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id IN($2,$3)`, team, admin, member)
	for _, actor := range []string{admin, member} {
		listed, err := service.List(ctx, actor)
		if err != nil || len(listed) != 0 {
			t.Fatalf("revoked member listed installation: %v", err)
		}
		if _, err = service.ListScoped(ctx, actor, scope, false); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("revoked scoped list: %v", err)
		}
		if _, err = service.SetPool(ctx, actor, "relay", Policy{Mode: "self-hosted-only", InstallationIDs: relayIDs}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("revoked member changed pool: %v", err)
		}
		for _, installation := range installations {
			var eligible bool
			if err = store.SQL().QueryRowContext(ctx, `SELECT paperboat.node_pool_eligible($1,$2,$3)`, installation.NodeID, actor, installation.Capability).Scan(&eligible); err != nil || eligible {
				t.Fatalf("revoked member eligible: %v", err)
			}
		}
	}
	persisted, err := service.Pool(ctx, admin, "relay")
	if err != nil || persisted.Mode != policy.Mode || len(persisted.InstallationIDs) != len(relayIDs) {
		t.Fatalf("own stored pool unavailable or changed after membership revocation: %v", err)
	}
	assertManagedRelay(false)
	if _, err = service.SetPool(ctx, admin, "relay", Policy{Mode: "mixed", InstallationIDs: []string{}}); err != nil {
		t.Fatalf("clear personal pool after team revocation: %v", err)
	}
	assertManagedRelay(true)
	exerciseTeamPreviewPlacement(t, store, service, owner, outsider, tunnel, suffix)
}

func exerciseTeamPreviewPlacement(t *testing.T, store *db.DB, service *Service, owner, outsider string, installation Installation, suffix string) {
	t.Helper()
	ctx := t.Context()
	preview, managed := "placement_preview_"+suffix, "placement_managed_"+suffix
	endpoint := testutil.EndpointLabel(preview)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,lease_deadline) VALUES('foreground',$1,$2,$3,$3,'machine','session','http','127.0.0.1:3000',now()+interval '10 minutes')`, preview, "https://"+endpoint+".preview.example.test", owner)
	t.Cleanup(func() {
		cleanup := context.Background()
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.selfhost_placements WHERE resource_kind='preview' AND resource_id=$1`, preview)
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.preview_domains WHERE preview_id=$1`, preview)
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.preview_leases WHERE id=$1`, preview)
		store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, managed)
	})
	exec(`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,1,$4,'exact','dns:test','verified','target.example.test','managed','ready','ready','clear')`, "placement_domain_"+suffix, owner, preview, "custom-"+endpoint+".example.test")
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at) VALUES($1,'managed','1.0','fixture_managed','ready',true,now())`, managed)
	if _, err := service.SetPlacement(ctx, outsider, "preview", preview, &installation.InstallationID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("outsider changed placement: %v", err)
	}
	placement, err := service.SetPlacement(ctx, owner, "preview", preview, &installation.InstallationID)
	if err != nil || placement.NodeID == nil || *placement.NodeID != installation.NodeID {
		t.Fatalf("explicit placement: %v", err)
	}
	eligible := func(node string, want bool) {
		t.Helper()
		var actual bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.node_preview_eligible($1,$2,$3)`, node, owner, preview).Scan(&actual); err != nil || actual != want {
			t.Fatalf("placement selection=%v want %v: %v", actual, want, err)
		}
	}
	eligible(installation.NodeID, false)
	var target, state, recordType string
	var generation int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT dns_target,ownership_state,expected_records->0->>'type',generation FROM paperboat.preview_domains WHERE preview_id=$1`, preview).Scan(&target, &state, &recordType, &generation); err != nil || target != "140.245.11.41" || state != "pending" || recordType != "A" || generation != 2 {
		t.Fatalf("placement DNS reset: %q %q %q %d %v", target, state, recordType, generation, err)
	}
	exec(`UPDATE paperboat.preview_domains SET ownership_state='verified' WHERE preview_id=$1`, preview)
	eligible(installation.NodeID, true)
	eligible(managed, false)
	if err = service.RemoveScoped(ctx, owner, installation.InstallationID, false); err != nil {
		t.Fatal(err)
	}
	eligible(installation.NodeID, false)
	eligible(managed, false)
	cleared, err := service.SetPlacement(ctx, owner, "preview", preview, nil)
	if err != nil || cleared.InstallationID != nil || cleared.NodeID != nil {
		t.Fatalf("clear placement: %v", err)
	}
	eligible(managed, true)
}
