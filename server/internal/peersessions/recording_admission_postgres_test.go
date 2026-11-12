package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
)

// A CLI session recording replay: the viewer's CLI endpoint reaches the team machine for capability recording while
// its admission lives and the policy still lets the viewer replay there; losing recording_view withdraws it. The
// machine is owned by the team or is a personal machine shared into it.
func TestRecordingReplayAdmissionReachesBothEndpointsOnPostgres(t *testing.T) {
	t.Run("team-owned", func(t *testing.T) { recordingReplayAdmission(t, false) })
	t.Run("shared personal machine", func(t *testing.T) { recordingReplayAdmission(t, true) })
}

func recordingReplayAdmission(t *testing.T, shared bool) {
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
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprint(time.Now().UnixNano())
	account, cli, machine, environment, team := "rec_user_"+suffix, "rec_cli_"+suffix, "rec_machine_"+suffix, "rec_env_"+suffix, "rec-team-"+suffix
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	execute(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, account)
	execute(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, account)
	execute(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'recording test','desktop','test',ARRAY['machines:connect'],'active',$3,$3)`, cli, account, now)
	execute(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, account)
	owner := &team
	if shared {
		owner = nil
	}
	execute(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation,owner_team_id) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1,$4)`, machine, account, environment, owner)
	if shared {
		execute(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, machine, account)
	}
	execute(`INSERT INTO paperboat.workspace_policies(team_id,version,source,digest) VALUES($1,1,'{}','test')`, team)
	allow := func() {
		execute(`INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports) SELECT id,0,false,'account',$2,'machine',$3,'recording_view','[1,65536)' FROM paperboat.workspace_policies WHERE team_id=$1`, team, account, machine)
	}
	allow()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "recording-test", PrivateKey: private}}, "recording-test", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(250 * time.Millisecond) }
	for _, endpoint := range []struct {
		id       string
		role     wire.Role
		roleName string
	}{{cli, wire.RoleCLI, "cli"}, {machine, wire.RoleMachine, "machine"}} {
		_, _, quic := networkEndpointTLS(t, endpoint.id, now)
		certificate, fingerprint := networkCertificateWithKeys(t, private, account, endpoint.role, endpoint.id, 1, 1, quic, now, now.Add(time.Hour))
		execute(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,$4,1,1,$5,$6,$7,$8,'recording-test',$9)`, fingerprint[:], account, endpoint.id, endpoint.roleName, certificate, quic[:], now, now.Add(time.Hour), public)
		key, wireguard := networkX25519Key(t)
		registration := NetworkRegistration{OperationID: "register_" + endpoint.id, UserID: account, EndpointID: endpoint.id, Role: endpoint.roleName, EndpointGeneration: 1, WireGuardPublicKey: wireguard, DiscoPublicKey: networkDiscoPublic(t, key), QUICCertificateFingerprint: fingerprint[:]}
		if endpoint.role == wire.RoleMachine {
			registration.MachineID, registration.MachineGeneration = machine, 1
		}
		if _, err := service.Register(ctx, registration); err != nil {
			t.Fatal(err)
		}
	}
	// Two tickets for the same recording: one scope, named by the recording, lasting as long as the later ticket.
	admissionID := "rec_01234567-89ab-cdef-0123-456789abcdef"
	execute(`INSERT INTO paperboat.recording_native_admissions(id,recording_id,account_id,native_cli_session_id,machine_id,expires_at) VALUES($1,$2,$3,$4,$5,$6),($1||'-2',$2,$3,$4,$5,$7)`, "token_rec_"+suffix, admissionID, account, cli, machine, now.Add(4*time.Minute), now.Add(5*time.Minute))
	live := []AdmittedScope{{ResourceKind: "recording", ResourceID: admissionID}}
	for _, endpoint := range []string{cli, machine} {
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "recording_" + suffix, UserID: account, EndpointID: endpoint})
		if err != nil {
			t.Fatal(err)
		}
		assertNetworkScope(t, result.Configuration, "recording", admissionID, "recording")
		scopes := 0
		for _, peer := range networkClaims(t, result.Configuration)["peers"].([]any) {
			for _, raw := range peer.(map[string]any)["scopes"].([]any) {
				if scope := raw.(map[string]any); scope["resource_kind"] == "recording" {
					scopes++
					if scope["expires_at"].(float64) != float64(now.Add(5*time.Minute).Unix()) {
						t.Fatalf("recording scope expiry %v", scope["expires_at"])
					}
				}
			}
		}
		if scopes != 1 {
			t.Fatalf("recording scopes = %d, want one per recording", scopes)
		}
	}
	// The host's revocation feed names a replay ticket only once its viewer may no longer replay there.
	edge := controlplane.NewEdgeService(store, "")
	jtis := []string{"token_rec_" + suffix, "token_rec_" + suffix + "-2"}
	revoked := func(step string, watched []string, want ...string) {
		t.Helper()
		got, err := edge.OperationRevocations(ctx, environment, watched)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("%s: revocations = %v, %v, want %v", step, got, err, want)
		}
	}
	revoked("allowed", jtis)
	// Past the ticket's expiry a replay the endpoint still runs keeps its scope; one it does not run loses it.
	execute(`UPDATE paperboat.recording_native_admissions SET expires_at=$2 WHERE recording_id=$1`, admissionID, now.Add(-time.Minute))
	for _, endpoint := range []string{cli, machine} {
		running, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "recording_running_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: live})
		if err != nil {
			t.Fatal(err)
		}
		assertNetworkScope(t, running.Configuration, "recording", admissionID, "recording")
		idle, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "recording_idle_" + suffix, UserID: account, EndpointID: endpoint})
		if err != nil {
			t.Fatal(err)
		}
		assertNoNetworkScope(t, idle.Configuration, "recording", admissionID, "recording")
	}
	execute(`DELETE FROM paperboat.workspace_policy_rules WHERE action='recording_view' AND target_id=$1`, machine)
	for _, endpoint := range []string{cli, machine} {
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "recording_revoked_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: live})
		if err != nil {
			t.Fatal(err)
		}
		assertNoNetworkScope(t, result.Configuration, "recording", admissionID, "recording")
	}
	// Expired tickets stay in the feed while the host still watches them (a running replay), not after.
	revoked("rule removed", jtis, jtis...)
	revoked("rule removed, nothing running", []string{})
	allow()
	revoked("rule restored", jtis)
	execute(`UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, team, account)
	revoked("member removed", jtis, jtis...)
	execute(`UPDATE paperboat.team_members SET active=true WHERE team_id=$1 AND account_id=$2`, team, account)
	revoked("member back", jtis)
	execute(`UPDATE paperboat.user_machines SET revoked_at=$2 WHERE id=$1`, machine, now) // last: revocation is final
	revoked("machine revoked", jtis, jtis...)
}
