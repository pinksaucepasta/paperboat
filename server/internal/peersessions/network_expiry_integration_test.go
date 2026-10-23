package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/admission"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
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
	execute(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machine, account, environment)
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
		role     wire.Role
		roleName string
	}{{cli, wire.RoleCLI, "cli"}, {machine, wire.RoleMachine, "machine"}} {
		_, _, quic := networkEndpointTLS(t, endpoint.id, now)
		certificate, fingerprint := networkCertificateWithKeys(t, private, account, endpoint.role, endpoint.id, 1, 1, quic, now, now.Add(time.Hour))
		execute(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,$4,1,1,$5,$6,$7,$8,'expiry-test',$9)`, fingerprint[:], account, endpoint.id, endpoint.roleName, certificate, quic[:], now, now.Add(time.Hour), public)
		key, wireguard := networkX25519Key(t)
		registration := NetworkRegistration{OperationID: "register_" + endpoint.id, UserID: account, EndpointID: endpoint.id, Role: endpoint.roleName, EndpointGeneration: 1, WireGuardPublicKey: wireguard, DiscoPublicKey: networkDiscoPublic(t, key), QUICCertificateFingerprint: fingerprint[:]}
		if endpoint.role == wire.RoleMachine {
			registration.MachineID, registration.MachineGeneration = machine, 1
		}
		if _, err := service.Register(ctx, registration); err != nil {
			t.Fatal(err)
		}
	}
	access := "expiry_access_" + suffix
	execute(`INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,capabilities,operation_id,expires_at) VALUES('external_issuer',$1,$2,$3,$4,$5,'https://machine.example.test',$6,$7,ARRAY['terminal','files','private_access'],$8,$9)`, access, machine, account, environment, cli, "terminal_"+suffix, "files_"+suffix, "operation_access_"+suffix, now.Add(5*time.Second))
	execute(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids) VALUES($1,'task23','v1',$2,'relay.example.test',443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,20,$3,$4,ARRAY[$5])`, "expiry_relay_"+suffix, "epoch_"+suffix, now, now.Add(time.Minute), account)
	inspector, assignment := addExpiryResourceAdmissions(t, execute, account, cli, machine, environment, suffix, now)
	live := []AdmittedScope{{ResourceKind: "machine_access", ResourceID: access}, {ResourceKind: "inspector", ResourceID: inspector}, {ResourceKind: "config_compare", ResourceID: assignment}}
	execute(`INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,capabilities,operation_id,expires_at)
 SELECT 'external_issuer',$1||'-historical-'||i,$2,$3,$4,$5,'https://machine.example.test',$6||i,$7||i,ARRAY['terminal','files','private_access'],$8||i,$9 FROM generate_series(1,200) i`, access, machine, account, environment, cli, "terminal_history_"+suffix, "files_history_"+suffix, "operation_history_"+suffix, now.Add(-time.Second))
	execute(`INSERT INTO config_compare_admissions(id,account_id,environment_id,machine_id,installation_generation,assignment_id,assignment_version,config_path,conflict_revision,expected_remote_revision,native_cli_session_id,source_machine_id,workspace_id,expires_at,operation_id)
 SELECT 'token_'||gen_random_uuid(),a.account_id,a.environment_id,a.machine_id,a.installation_generation,a.assignment_id,a.assignment_version,a.config_path,a.conflict_revision,a.expected_remote_revision,a.native_cli_session_id,a.source_machine_id,a.workspace_id,$2,a.operation_id||'-history-'||i
 FROM config_compare_admissions a CROSS JOIN generate_series(1,200) i WHERE a.assignment_id=$1`, assignment, now.Add(-time.Second))
	for _, remaining := range []time.Duration{-time.Second, 750 * time.Millisecond, 5 * time.Second} {
		execute(`UPDATE paperboat.user_machine_access_sessions SET expires_at=$2 WHERE id=$1`, access, now.Add(remaining))
		execute(`UPDATE inspector_credentials SET expires_at=$2 WHERE credential_id=$1`, inspector, now.Add(remaining))
		execute(`UPDATE config_compare_admissions SET expires_at=$2 WHERE assignment_id=$1`, assignment, now.Add(remaining))
		for _, endpoint := range []string{cli, machine} {
			result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "configuration_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: live})
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
			if claims["exp"].(float64) != float64(now.Add(admission.DefaultLifetime).Unix()) {
				t.Fatal("application scope shortened unrelated network binding")
			}
			assertNetworkScope(t, result.Configuration, "inspector", inspector, "inspector")
			assertNetworkScope(t, result.Configuration, "config_compare", assignment, "config_compare")
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
			if applications != 3 {
				t.Fatalf("application scopes at wire expiry: %d", applications)
			}
		}
	}
	execute(`UPDATE user_machine_access_sessions SET expires_at=$2 WHERE id=$1`, access, now.Add(-time.Second))
	for _, endpoint := range []string{cli, machine} {
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "inactive_history_" + suffix, UserID: account, EndpointID: endpoint})
		if err != nil {
			t.Fatalf("inactive credential history exhausted bounded configuration: %v", err)
		}
		assertNoNetworkScope(t, result.Configuration, "machine_access", access, "terminal")
	}
	var generationBefore, generationAfter int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT config_generation FROM peer_network_identities WHERE endpoint_id=$1`, cli).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}
	for name, refs := range map[string][]AdmittedScope{
		"duplicate":        {live[0], live[0]},
		"unsupported kind": {{ResourceKind: "account", ResourceID: account}},
		"oversized id":     {{ResourceKind: "machine_access", ResourceID: strings.Repeat("a", 257)}},
		"over budget":      make([]AdmittedScope, 129),
	} {
		_, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "invalid_refs_" + suffix, UserID: account, EndpointID: cli, AdmittedScopes: refs})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s continuation input accepted: %v", name, err)
		}
	}

	for name, refs := range map[string][]AdmittedCertificate{
		"duplicate certificates": {{EndpointID: cli, Fingerprint: strings.Repeat("ab", 32)}, {EndpointID: cli, Fingerprint: strings.Repeat("ab", 32)}},
		"uppercase certificate":  {{EndpointID: cli, Fingerprint: strings.Repeat("AB", 32)}},
		"invalid certificate":    {{EndpointID: cli, Fingerprint: "not-a-fingerprint"}},
		"oversized endpoint":     {{EndpointID: strings.Repeat("a", 129), Fingerprint: strings.Repeat("ab", 32)}},
		"certificate budget":     make([]AdmittedCertificate, 129),
	} {
		if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "invalid_cert_" + suffix, UserID: account, EndpointID: cli, AdmittedScopes: live, AdmittedCertificates: refs}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "no_scope_cert_" + suffix, UserID: account, EndpointID: cli, AdmittedCertificates: []AdmittedCertificate{{EndpointID: cli, Fingerprint: strings.Repeat("ab", 32)}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("certificate continuation without live scopes accepted: %v", err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT config_generation FROM peer_network_identities WHERE endpoint_id=$1`, cli).Scan(&generationAfter); err != nil || generationAfter != generationBefore {
		t.Fatal("invalid continuation request changed network authority")
	}

	// Expired endpoint certificates gate a new transport, not admitted authority.
	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	var networkID string
	for _, endpoint := range []string{cli, machine} {
		if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "fresh_expired_cert_" + suffix, UserID: account, EndpointID: endpoint}); !errors.Is(err, ErrDenied) {
			t.Fatalf("new transport accepted expired certificate: %v", err)
		}
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "admitted_expired_cert_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: live})
		if err != nil {
			t.Fatalf("admitted authority ended at endpoint certificate TTL: %v", err)
		}
		assertNetworkScope(t, result.Configuration, "machine_access", access, "terminal")
		for _, peer := range networkClaims(t, result.Configuration)["peers"].([]any) {
			for _, raw := range peer.(map[string]any)["scopes"].([]any) {
				scope := raw.(map[string]any)
				if scope["resource_kind"] == "machine_network" {
					networkID = scope["resource_id"].(string)
				}
			}
		}
		if networkID == "" {
			t.Fatal("admitted peer carrier lost authority at certificate TTL")
		}
	}
	service.now = func() time.Time { return now.Add(250 * time.Millisecond) }

	execute(`UPDATE inspector_credentials SET revoked_at=now() WHERE credential_id=$1`, inspector)
	execute(`UPDATE control_config_assignments SET version=version+1 WHERE id=$1`, assignment)
	// Explicit withdrawal reaches both the caller and the machine even after TTL.
	execute(`UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=now(),revocation_reason='test' WHERE id=$1`, access)
	for _, endpoint := range []string{cli, machine} {
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "revoked_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: live})
		if err != nil {
			t.Fatal(err)
		}
		assertNoNetworkScope(t, result.Configuration, "machine_access", access, "terminal")
		assertNoNetworkScope(t, result.Configuration, "machine_access", access, "file_transfer")
		assertNoNetworkScope(t, result.Configuration, "machine_access", access, "private_access")
		assertNoNetworkScope(t, result.Configuration, "inspector", inspector, "inspector")
		assertNoNetworkScope(t, result.Configuration, "config_compare", assignment, "config_compare")
	}
	// The idle admitted carrier also survives certificate TTL after app work ends.
	service.now = func() time.Time { return now.Add(2 * time.Hour) }
	carrierRef := []AdmittedScope{{ResourceKind: "machine_network", ResourceID: networkID}}
	for _, endpoint := range []string{cli, machine} {
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "idle_carrier_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: carrierRef})
		if err != nil {
			t.Fatalf("idle admitted carrier expired: %v", err)
		}
		assertNetworkScope(t, result.Configuration, "machine_network", networkID, "connect")
	}

	// Same-key renewal changes the admission certificate, not the identity of a
	// carrier that already authenticated the old exact certificate.
	var oldFingerprint, quicBytes, wg, disco []byte
	if err := store.SQL().QueryRowContext(ctx, `SELECT n.quic_certificate_fingerprint,c.quic_public_key,n.wireguard_public_key,n.disco_public_key FROM peer_network_identities n JOIN peer_endpoint_certificates c ON c.fingerprint=n.quic_certificate_fingerprint WHERE n.endpoint_id=$1`, cli).Scan(&oldFingerprint, &quicBytes, &wg, &disco); err != nil {
		t.Fatal(err)
	}
	var quic [32]byte
	copy(quic[:], quicBytes)
	renewed, renewedFingerprint := networkCertificateWithKeys(t, private, account, wire.RoleCLI, cli, 1, 2, quic, now.Add(2*time.Hour), now.Add(4*time.Hour))
	execute(`UPDATE peer_endpoint_certificates SET revoked_at=$2,revocation_reason='certificate_superseded' WHERE endpoint_id=$1`, cli, now.Add(2*time.Hour))
	execute(`INSERT INTO peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,'cli',1,2,$4,$5,$6,$7,'expiry-test',$8)`, renewedFingerprint[:], account, cli, renewed, quic[:], now.Add(2*time.Hour), now.Add(4*time.Hour), public)
	for _, endpoint := range []string{cli, machine} {
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "rebind_gap_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: carrierRef})
		if err != nil {
			t.Fatalf("same-key renewal gap revoked admitted carrier: %v", err)
		}
		assertNetworkScope(t, result.Configuration, "machine_network", networkID, "connect")
	}
	if _, err := service.Register(ctx, NetworkRegistration{OperationID: "renew_" + suffix, UserID: account, EndpointID: cli, Role: "cli", EndpointGeneration: 1, ExpectedKeyGeneration: 1, WireGuardPublicKey: wg, DiscoPublicKey: disco, QUICCertificateFingerprint: renewedFingerprint[:]}); err != nil {
		t.Fatal(err)
	}
	oldHex := hex.EncodeToString(oldFingerprint)
	checkPinned := func(endpoint string, refs []AdmittedCertificate, want bool) {
		t.Helper()
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "pinned_" + suffix, UserID: account, EndpointID: endpoint, AdmittedScopes: carrierRef, AdmittedCertificates: refs})
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(result.Configuration, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil || !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
			t.Fatal("pinned binding signature invalid")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Self  networkBinding `json:"self"`
			Peers []networkPeer  `json:"peers"`
		}
		if err := json.Unmarshal(payload, &document); err != nil {
			t.Fatal(err)
		}
		binding := document.Self
		if endpoint != cli {
			for _, peer := range document.Peers {
				if peer.Identity.EndpointID == cli {
					binding = peer.Identity
				}
			}
		}
		if binding.QUICCertificateFingerprint != hex.EncodeToString(renewedFingerprint[:]) {
			t.Fatal("fresh binding did not use renewed certificate")
		}
		if got := len(binding.AdmittedCertificateFingerprints) == 1 && binding.AdmittedCertificateFingerprints[0] == oldHex; got != want {
			t.Fatalf("exact old admitted binding retained=%v want=%v", got, want)
		}
	}
	pinned := []AdmittedCertificate{{EndpointID: cli, Fingerprint: oldHex}}
	for _, endpoint := range []string{cli, machine} {
		checkPinned(endpoint, pinned, true)
		checkPinned(endpoint, nil, false)
		checkPinned(endpoint, []AdmittedCertificate{{EndpointID: cli, Fingerprint: strings.Repeat("ab", 32)}}, false)
	}
	execute(`UPDATE peer_endpoint_certificates SET revocation_reason='key_compromise' WHERE fingerprint=$1`, oldFingerprint)
	for _, endpoint := range []string{cli, machine} {
		checkPinned(endpoint, pinned, false)
	}
	execute(`UPDATE peer_endpoint_certificates SET revoked_at=now(),revocation_reason='key_compromise' WHERE endpoint_id=$1`, cli)
	if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "revoked_cert_" + suffix, UserID: account, EndpointID: cli, AdmittedScopes: carrierRef}); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked endpoint certificate retained authority: %v", err)
	}
	result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "revoked_peer_cert_" + suffix, UserID: account, EndpointID: machine, AdmittedScopes: carrierRef})
	if err != nil {
		t.Fatal(err)
	}
	assertNoNetworkScope(t, result.Configuration, "machine_network", networkID, "connect")

}

func addExpiryResourceAdmissions(t *testing.T, execute func(string, ...any), account, cli, machine, environment, suffix string, now time.Time) (string, string) {
	t.Helper()
	tunnel, route, inspector, repository, assignment := "expiry_tunnel_"+suffix, "expiry_route_"+suffix, "expiry_inspector_"+suffix, "expiry_repository_"+suffix, "expiry_assignment_"+suffix
	endpoint := uuid.NewString()
	execute(`INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,'active','private',1,$3,$4,$5,$2)`, tunnel, account, "endpoint_"+endpoint, "https://"+endpoint+".tunnels.example.test", machine)
	execute(`INSERT INTO tunnel_routes(id,tunnel_id,name,priority,protocol,match_type,origin_scheme,origin_address,tls_verification,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'expiry',1,'http','catch_all','http','127.0.0.1:3000','not_applicable',1,'active',$3,$3,$4,$4)`, route, tunnel, account, now)
	hash := sha256.Sum256([]byte(inspector))
	execute(`INSERT INTO inspector_credentials(credential_id,token_hash,account_id,owner_account_id,resource_kind,resource_id,route_id,action,resource_generation,route_generation,target_generation,issued_at,expires_at,native_cli_session_id,native_machine_id,native_machine_generation) VALUES($1,$2,$3,$3,'tunnel',$4,$5,'inspect',1,1,1,$6,$7,$8,$9,1)`, inspector, hash[:], account, tunnel, route, now.Add(-10*time.Second), now.Add(5*time.Second), cli, machine)
	execute(`INSERT INTO control_config_repositories(id,owner_user_id,provider,external_ref,display_name,state) VALUES($1,$2,'github',$1,'owner/config','active')`, repository, account)
	execute(`INSERT INTO control_config_assignments(id,machine_id,environment_id,repository_id,consent_state,warning_revision,accepted_at,configuration_revision) VALUES($1,$2,$3,$4,'accepted','warning',now(),repeat('a',64))`, assignment, machine, environment, repository)
	execute(`INSERT INTO control_config_sync_statuses(environment_id,repository_id,assignment_id,machine_id,installation_generation,warning_revision,policy_revision,sync_revision,state,remote_revision,conflicts,machine_updated_at,observed_at) VALUES($1,$2,$3,$4,1,'warning','policy',1,'conflict','remote',jsonb_build_array(jsonb_build_object('path','editor/settings','revision',repeat('a',64))),now(),now())`, environment, repository, assignment, machine)
	execute(`INSERT INTO config_compare_admissions(id,account_id,environment_id,machine_id,installation_generation,assignment_id,assignment_version,config_path,conflict_revision,expected_remote_revision,native_cli_session_id,source_machine_id,workspace_id,expires_at,operation_id) VALUES($1,$2,$3,$4,1,$5,1,'editor/settings',repeat('a',64),'remote',$6,$4,'personal',$7,$8)`, "token_"+uuid.NewString(), account, environment, machine, assignment, cli, now.Add(5*time.Second), "operation_"+suffix)
	return inspector, assignment
}
