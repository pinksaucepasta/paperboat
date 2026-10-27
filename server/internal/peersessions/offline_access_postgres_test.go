package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
)

// F21-11 (PLAN 3b offline-capable authority): a host's document carries, for a daemon that asks for
// AuthorityUntilRevoked, one machine_access "accept" scope per capability each peer's account may start on it, and
// those scopes equal the online descriptor decision (machine_capability_allowed) for every (account, capability).
// Its mesh scopes are until revoked (expires_at 0, F21-01). A daemon that does not ask gets the earlier shape.
func TestOfflineSessionRightsEqualTheDescriptorDecisionOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, outsider := "offline_owner_"+suffix, "offline_member_"+suffix, "offline_outsider_"+suffix
	previewMember := "offline_preview_" + suffix
	revokedMember, suspendedMember := "offline_revoked_"+suffix, "offline_suspended_"+suffix
	host, team := "offline_host_"+suffix, "offline-team-"+suffix
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "offline-access", PrivateKey: signer}}, "offline-access", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{owner, member, outsider, revokedMember, suspendedMember, previewMember} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, "env_"+host, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, host, owner, "env_"+host)
	exec(`SELECT paperboat.ensure_machine_address_leases((SELECT user_id FROM paperboat.user_machines WHERE id=$1), ARRAY[$1]::text[])`, host)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 hour')`, "billing_"+team, team)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true),($1,$4,1,'member',true),($1,$5,1,'member',true),($1,$6,1,'member',true)`, team, owner, member, revokedMember, suspendedMember, previewMember)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, host, owner)
	for _, account := range []string{member, revokedMember, suspendedMember} {
		teamstest.SetMachineAccess(t, store, team, host, account, "terminal")
	}
	// preview_manage is the machine capability nearest to inspecting; it must not confer the offline inspector right.
	teamstest.SetMachineAccess(t, store, team, host, previewMember, "preview_manage")

	register := func(account, endpoint string, role wire.Role) {
		name := map[wire.Role]string{wire.RoleCLI: "cli", wire.RoleMachine: "machine"}[role]
		t.Helper()
		_, _, quic := networkEndpointTLS(t, endpoint, now)
		certificate, fingerprint := networkCertificateWithKeys(t, signer, account, role, endpoint, 1, 1, quic, now, now.Add(time.Hour))
		exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,$4,1,1,$5,$6,$7,$8,'offline-access',$9)`, fingerprint[:], account, endpoint, name, certificate, quic[:], now, now.Add(time.Hour), signer.Public().(ed25519.PublicKey))
		private, public := networkX25519Key(t)
		request := NetworkRegistration{OperationID: "register_" + endpoint, UserID: account, EndpointID: endpoint, Role: name, EndpointGeneration: 1, WireGuardPublicKey: public, DiscoPublicKey: networkDiscoPublic(t, private), QUICCertificateFingerprint: fingerprint[:]}
		if role == wire.RoleMachine {
			request.MachineID, request.MachineGeneration = endpoint, 1
		}
		if _, err := service.Register(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	// An eligible relay: the configuration carries relay grants, which embed the peers' scopes.
	relayNode := "offline_relay_" + suffix
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids)
 VALUES($1,'offline','v1','epoch','relay.example.test',443,444,'ready',true,now(),1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,1,now(),now()+interval '1 hour',ARRAY[$2])`, relayNode, owner)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, relayNode)
	})
	register(owner, host, wire.RoleMachine)
	clis := map[string]string{}
	for _, account := range []string{owner, member, outsider, revokedMember, suspendedMember} {
		cli := "offline_cli_" + account
		clis[cli] = account
		exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'offline test','desktop','test',ARRAY['machines:connect'],'active',$3,$3)`, cli, account, now)
		register(account, cli, wire.RoleCLI)
	}

	// scopes returns peer endpoint -> sorted offline capabilities, and the mesh scopes' expiries, of one document.
	read := func(authority string) (map[string][]string, []float64) {
		t.Helper()
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "configuration_" + suffix + authority, UserID: owner, EndpointID: host, Authority: authority})
		if err != nil {
			t.Fatal(err)
		}
		// The relay grants were signed (mint refuses a scope without an expiry): until-revoked scopes end with the grant.
		if len(result.RelayGrants) == 0 {
			t.Fatalf("authority %q: no relay grants", authority)
		}
		offline, mesh := map[string][]string{}, []float64{}
		for _, raw := range networkClaims(t, result.Configuration)["peers"].([]any) {
			peer := raw.(map[string]any)
			endpoint := peer["identity"].(map[string]any)["endpoint_id"].(string)
			for _, rawScope := range peer["scopes"].([]any) {
				scope := rawScope.(map[string]any)
				switch {
				case scope["resource_kind"] == "machine_access" && scope["resource_id"] == host:
					if scope["direction"] != "accept" || scope["expires_at"].(float64) != 0 || scope["resource_generation"].(float64) < 1 {
						t.Fatalf("offline scope shape: %v", scope)
					}
					offline[endpoint] = append(offline[endpoint], scope["capability"].(string))
				case scope["resource_kind"] == "machine_network":
					mesh = append(mesh, scope["expires_at"].(float64))
				}
			}
			slices.Sort(offline[endpoint])
		}
		return offline, mesh
	}

	offline, mesh := read(AuthorityUntilRevoked)
	for cli, account := range clis {
		var want []string
		for _, c := range offlineCapabilities {
			if slices.Contains(offlineOwnerOnly, c[1]) && account != owner {
				continue // a team grant never confers it offline
			}
			var allowed bool
			if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.machine_capability_allowed($1,$2,$3)`, account, host, c[0]).Scan(&allowed); err != nil {
				t.Fatal(err)
			}
			if allowed {
				want = append(want, c[1])
			}
		}
		slices.Sort(want)
		if !slices.Equal(offline[cli], want) {
			t.Errorf("%s (%s): offline scopes %v, descriptor decision %v", cli, account, offline[cli], want)
		}
	}
	if len(offline["offline_cli_"+member]) != 1 || offline["offline_cli_"+member][0] != "terminal" {
		t.Errorf("member granted terminal only has %v", offline["offline_cli_"+member])
	}
	if !slices.Contains(offline["offline_cli_"+owner], "inspector") {
		t.Errorf("the host's own account lacks the inspector right: %v", offline["offline_cli_"+owner])
	}
	if got := offline["offline_cli_"+previewMember]; len(got) != 0 {
		t.Errorf("a team member granted preview_manage got offline scopes %v, want none (inspect is decided per resource)", got)
	}
	if len(offline["offline_cli_"+outsider]) != 0 {
		t.Errorf("an account without access has offline scopes %v", offline["offline_cli_"+outsider])
	}
	if len(mesh) == 0 || slices.ContainsFunc(mesh, func(expiry float64) bool { return expiry != 0 }) {
		t.Errorf("mesh scopes for an until-revoked daemon %v, want all 0", mesh)
	}

	// Each refusal uses its own member: disabling an account or suspending a team also fires revoke triggers.
	for _, account := range []string{member, revokedMember, suspendedMember} {
		if got := offline["offline_cli_"+account]; len(got) != 1 || got[0] != "terminal" {
			t.Fatalf("%s granted terminal only has %v", account, got)
		}
	}
	// Each change that decides an offline right moves the host's peer_network topic (migration 196), so the host
	// refreshes its document; an update that leaves the deciding column as it was moves nothing (no flapping).
	topic := func() int64 {
		t.Helper()
		var g int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='peer_network'),0)
 + coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=$2 AND topic='peer_network'),0)`, host, owner).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	moves := func(what string, want bool, change func()) {
		t.Helper()
		before := topic()
		change()
		if moved := topic() != before; moved != want {
			t.Errorf("%s: host peer_network moved=%v, want %v", what, moved, want)
		}
	}
	moves("same status written again", false, func() { exec(`UPDATE paperboat.users SET status=status WHERE id=$1`, member) })
	moves("same suspension written again", false, func() { exec(`UPDATE paperboat.teams SET suspended=suspended WHERE team_id=$1`, team) })
	// A disabled account has no offline rights (online, its sessions are refused).
	moves("member disabled", true, func() { exec(`UPDATE paperboat.users SET status='disabled' WHERE id=$1`, member) })
	// A revoked grant leaves the member's next document without the scope.
	moves("grant revoked", true, func() { teamstest.SetMachineAccess(t, store, team, host, revokedMember) })
	after, _ := read(AuthorityUntilRevoked)
	if got := after["offline_cli_"+member]; len(got) != 0 {
		t.Errorf("a disabled member still has offline scopes %v", got)
	}
	if got := after["offline_cli_"+revokedMember]; len(got) != 0 {
		t.Errorf("member scopes after the grant was revoked: %v", got)
	}
	if got := after["offline_cli_"+suspendedMember]; len(got) != 1 {
		t.Fatalf("an unaffected member lost its scopes: %v", got)
	}
	// A suspended team gives none (no trigger stands behind an offline right).
	moves("team suspended", true, func() {
		exec(`UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, team, owner)
	})
	if after, _ := read(AuthorityUntilRevoked); len(after["offline_cli_"+suspendedMember]) != 0 {
		t.Errorf("a suspended team's member still has offline scopes %v", after["offline_cli_"+suspendedMember])
	}

	// A machine shared out of the team (or back in) ends (or starts) every member's rights on it.
	moves("binding written again", false, func() {
		exec(`UPDATE paperboat.team_resource_bindings SET active=active WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2`, team, host)
	})
	moves("machine unshared from the team", true, func() {
		exec(`UPDATE paperboat.team_resource_bindings SET active=false WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2`, team, host)
	})
	moves("machine shared into the team again", true, func() {
		exec(`UPDATE paperboat.team_resource_bindings SET active=true WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2`, team, host)
	})
	moves("binding deleted", true, func() {
		exec(`DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2`, team, host)
	})

	// An earlier daemon gets no offline scopes and expiring mesh scopes.
	legacy, legacyMesh := read("")
	if len(legacy) != 0 || slices.Contains(legacyMesh, 0) {
		t.Errorf("earlier daemon: offline %v, mesh expiries %v", legacy, legacyMesh)
	}
	if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "configuration_unknown_" + suffix, UserID: owner, EndpointID: host, Authority: "until_revoked_v9"}); err != ErrInvalid {
		t.Errorf("unknown authority: %v, want ErrInvalid", err)
	}
	// A host whose environment is revoked gets no document at all, so no offline rights.
	exec(`UPDATE paperboat.control_environments SET revoked_at=now() WHERE id=$1`, "env_"+host)
	if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "configuration_revoked_env_" + suffix, UserID: owner, EndpointID: host, Authority: AuthorityUntilRevoked}); !errors.Is(err, ErrDenied) {
		t.Errorf("a host with a revoked environment: %v, want ErrDenied", err)
	}
}
