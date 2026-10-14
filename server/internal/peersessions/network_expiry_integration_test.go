package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
)

func TestNetworkApplicationExpiryOnPostgres(t *testing.T) {
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
	account, cli, machine, environment := "expiry_user_"+suffix, "expiry_cli_"+suffix, "expiry_machine_"+suffix, "expiry_env_"+suffix
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	execute(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'expiry test','desktop','test',ARRAY['machines:connect'],'active',$3,$3)`, cli, account, now)
	execute(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, account)
	execute(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'expiry test','linux','amd64','/workspace','online','occupied',true,1)`, machine, account, environment)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "expiry-test", PrivateKey: private}}, "expiry-test", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test", NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(250 * time.Millisecond) }
	for _, endpoint := range []struct {
		id       string
		role     peeridentity.Role
		roleName string
	}{{cli, peeridentity.RoleCLI, "cli"}, {machine, peeridentity.RoleMachine, "machine"}} {
		_, _, quic := networkEndpointTLS(t, endpoint.id, now)
		certificate, fingerprint := networkCertificateWithKeys(t, private, account, endpoint.role, endpoint.id, 1, 1, quic, now, now.Add(time.Hour))
		execute(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,$4,1,1,$5,$6,$7,$8,'expiry-test',$9)`, fingerprint[:], account, endpoint.id, endpoint.roleName, certificate, quic[:], now, now.Add(time.Hour), public)
		key, wireguard := networkX25519Key(t)
		registration := NetworkRegistration{OperationID: "register_" + endpoint.id, UserID: account, EndpointID: endpoint.id, Role: endpoint.roleName, EndpointGeneration: 1, WireGuardPublicKey: wireguard, DiscoPublicKey: networkDiscoPublic(t, key), QUICCertificateFingerprint: fingerprint[:]}
		if endpoint.role == peeridentity.RoleMachine {
			registration.MachineID, registration.MachineGeneration = machine, 1
		}
		if _, err := service.Register(ctx, registration); err != nil {
			t.Fatal(err)
		}
	}
	access := "expiry_access_" + suffix
	execute(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,capabilities,operation_id,expires_at) VALUES('external_issuer',$1,$2,$3,$4,$5,'https://machine.example.test',$6,$7,ARRAY['terminal','files','private_access'],$8,$9)`, access, machine, account, environment, cli, "terminal_"+suffix, "files_"+suffix, "operation_access_"+suffix, now.Add(5*time.Second))
	execute(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids) VALUES($1,'task23','v1',$2,'relay.example.test',443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,20,$3,$4,ARRAY[$5])`, "expiry_relay_"+suffix, "epoch_"+suffix, now, now.Add(time.Minute), account)
	for _, remaining := range []time.Duration{750 * time.Millisecond, 5 * time.Second} {
		execute(`UPDATE paperboat.user_machine_access_sessions SET expires_at=$2 WHERE id=$1`, access, now.Add(remaining))
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "configuration_" + suffix, UserID: account, EndpointID: cli})
		if err != nil {
			t.Fatalf("application expiry %s: %v", remaining, err)
		}
		if len(result.RelayGrants) != 1 {
			t.Fatal("registered relay missing from signed configuration")
		}
		relayParts := strings.Split(result.RelayGrants[0], ".")
		if len(relayParts) != 3 {
			t.Fatal("invalid signed relay grant")
		}
		relaySignature, relayErr := base64.RawURLEncoding.DecodeString(relayParts[2])
		if relayErr != nil || !ed25519.Verify(public, []byte(relayParts[0]+"."+relayParts[1]), relaySignature) {
			t.Fatal("relay signature invalid")
		}
		parts := strings.Split(result.Configuration, ".")
		if len(parts) != 3 {
			t.Fatal("invalid signed network document")
		}
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil || !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
			t.Fatal("network signature invalid")
		}
		claims := networkClaims(t, result.Configuration)
		if claims["exp"].(float64) != float64(now.Add(mint.MaxProofTTL).Unix()) {
			t.Fatal("application scope shortened unrelated network binding")
		}
		applications := 0
		for _, peer := range claims["peers"].([]any) {
			for _, raw := range peer.(map[string]any)["scopes"].([]any) {
				scope := raw.(map[string]any)
				if scope["resource_kind"] != "machine_access" {
					continue
				}
				applications++
				if scope["expires_at"].(float64) != float64(now.Add(remaining).Unix()) {
					t.Fatal("application authorization lifetime extended")
				}
			}
		}
		if remaining < time.Second && applications != 0 || remaining >= time.Second && applications != 3 {
			t.Fatalf("application scopes at wire expiry: %d", applications)
		}
	}
}
