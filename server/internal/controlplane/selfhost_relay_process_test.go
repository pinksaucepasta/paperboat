package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// This bridge reuses the relay module's real QUIC carrier/replay test in a
// separate process, replacing its fake control responses with registered
// installation authority. It never builds or deploys a backend itself.
func TestSelfhostRegisteredRelayProcessOnPostgres(t *testing.T) {
	binary, dsn := os.Getenv("PAPERBOAT_SELFHOST_RELAY_TEST_BINARY"), os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if binary == "" || dsn == "" {
		t.Skip("requires isolated PostgreSQL and prebuilt relay lifecycle test executable")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	database, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if e = db.Migrate(ctx, database); e != nil {
		t.Fatal(e)
	}
	token := func(n int) string { b := make([]byte, n); rand.Read(b); return base64.RawURLEncoding.EncodeToString(b) }
	account, endpoint := "shr_"+token(12), "shcli_"+token(12)
	s := selfhost.New(database, strings.Repeat("s", 32))
	public, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	installed := selfhost.Registered{InstallationID: "shr_i_" + token(12), NodeID: "shr_n_" + token(12), RuntimeCredential: token(32), NodeGeneration: 1}
	installed.EdgePool = installed.InstallationID
	encrypted, e := secrets.Encrypt(strings.Repeat("s", 32), installed.RuntimeCredential)
	if e != nil {
		t.Fatal(e)
	}
	_, e = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,region,failure_domain,roles,transports,capacity_limit,capacity_observed_at,registry_expires_at,tls_spki_sha256) VALUES($1,$2,'1.0','initial_fixture','relay.example.test',443,444,'registered','test','test-process',ARRAY['relay'],ARRAY['derp_quic','derp_wss'],256,now(),now()+interval '1 hour',$3)`, installed.NodeID, installed.InstallationID, make([]byte, 32))
	if e != nil {
		t.Fatal(e)
	}
	_, e = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$4,$5,'Process fixture','relay','account',$6,$1,true)`, installed.InstallationID, installed.NodeID, []byte(public), sha256sumRuntime(installed.RuntimeCredential), encrypted, account)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		for _, q := range []string{`DELETE FROM paperboat.selfhost_pool_policies WHERE $1=ANY(installation_ids)`, `DELETE FROM paperboat.selfhost_installations WHERE id=$1`} {
			if _, e := database.SQL().ExecContext(cleanup, q, installed.InstallationID); e != nil {
				t.Errorf("installation cleanup: %v", e)
			}
		}
		if _, e := database.SQL().ExecContext(cleanup, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, installed.NodeID); e != nil {
			t.Errorf("node cleanup: %v", e)
		}
		for _, q := range []string{`DELETE FROM paperboat.peer_network_identities WHERE user_id=$1`, `DELETE FROM paperboat.peer_endpoint_certificates WHERE user_id=$1`, `DELETE FROM paperboat.account_e2ee_keys WHERE user_id=$1`, `DELETE FROM paperboat.users WHERE id=$1`} {
			if _, e := database.SQL().ExecContext(cleanup, q, account); e != nil {
				t.Errorf("account cleanup: %v", e)
			}
		}
	})
	// These are authoritative SQL fixture identities, not a claim of exercising
	// account enrollment. The new boundary under test starts at node registration.
	keyID := "aek_" + token(18)
	fingerprint := sha256.Sum256(public)
	certificateFingerprint := sha256.Sum256([]byte(endpoint))
	wireguard := make([]byte, 32)
	rand.Read(wireguard)
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, []any{account, account + "@example.test"}},
		{`INSERT INTO paperboat.account_e2ee_roots(user_id,public_key,fingerprint) VALUES($1,$2,$3)`, []any{account, []byte(public), fingerprint[:]}},
		{`INSERT INTO paperboat.account_e2ee_keys(key_id,user_id,public_key,fingerprint) VALUES($1,$2,$3,$4)`, []any{keyID, account, []byte(public), fingerprint[:]}},
		{`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id) VALUES($1,$2,$3,'cli',1,1,$4,$5,now(),now()+interval '1 hour',$6)`, []any{certificateFingerprint[:], account, endpoint, make([]byte, 172), wireguard, keyID}},
		{`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,config_generation,relay_revocation_generation,created_at,updated_at) VALUES($1,$2,'cli',1,0,1,$3,$4,('fd7a:115c:a1e0::'::inet+nextval('paperboat.peer_network_virtual_address_seq')),1,1,now(),now())`, []any{account, endpoint, wireguard, certificateFingerprint[:]}},
	} {
		if _, e = database.SQL().ExecContext(ctx, statement.q, statement.args...); e != nil {
			t.Fatal(e)
		}
	}
	assertEligible := func(want bool) {
		t.Helper()
		var actual bool
		if e := database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_pool_eligible($1,$2,'relay')`, installed.NodeID, account).Scan(&actual); e != nil || actual != want {
			t.Fatalf("pool eligibility %v want %v: %v", actual, want, e)
		}
	}
	if _, e = s.SetPool(ctx, account, "relay", selfhost.Policy{Mode: "mixed", InstallationIDs: []string{}}); e != nil {
		t.Fatal(e)
	}
	assertEligible(false)
	if _, e = s.SetPool(ctx, account, "relay", selfhost.Policy{Mode: "mixed", InstallationIDs: []string{installed.InstallationID}}); e != nil {
		t.Fatal(e)
	}
	assertEligible(true)
	edge := NewEdgeService(database, strings.Repeat("managed", 8))
	edge.SetNodeCredentials(s)
	control := edge.Handler()
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test/native-relay/revoke" {
			if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+installed.RuntimeCredential {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			if _, e := s.SetPool(r.Context(), account, "relay", selfhost.Policy{Mode: "mixed", InstallationIDs: []string{}}); e != nil {
				http.Error(w, "revocation failed", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(204)
			return
		}
		control.ServeHTTP(w, r)
	}))
	defer api.Close()
	restarted := selfhost.New(database, strings.Repeat("s", 32))
	t.Run("registered carrier revocation", func(t *testing.T) {
		var subjectGeneration uint64
		if e = database.SQL().QueryRowContext(ctx, `SELECT relay_revocation_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, account, endpoint).Scan(&subjectGeneration); e != nil {
			t.Fatal(e)
		}
		fixture := struct {
			URL, Credential, NodeID, AccountID, EndpointID string
			Generation, SubjectGeneration                  uint64
			CA                                             []byte
		}{api.URL, installed.RuntimeCredential, installed.NodeID, account, endpoint, 1, subjectGeneration, api.TLS.Certificates[0].Certificate[0]}
		raw, _ := json.Marshal(fixture)
		fixturePath := filepath.Join(t.TempDir(), "lifecycle.json")
		if e = os.WriteFile(fixturePath, raw, 0600); e != nil {
			t.Fatal(e)
		}
		command := exec.CommandContext(ctx, binary, "-test.run=^TestNativeRelayLifecycleRevokesConnectedCarrier$", "-test.count=1", "-test.timeout=30s")
		command.Env = append(os.Environ(), "PAPERBOAT_RELAY_LIFECYCLE_FIXTURE="+fixturePath)
		output, e := command.CombinedOutput()
		if e != nil {
			t.Fatalf("registered relay carrier lifecycle failed: %v\n%s", e, output)
		}
		assertEligible(false)
		var floor int64
		if e = database.SQL().QueryRowContext(ctx, `SELECT relay_revocation_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, account, endpoint).Scan(&floor); e != nil || floor <= int64(subjectGeneration) {
			t.Fatalf("revocation floor not persisted: %d %v", floor, e)
		}
		// Restoring selection preserves the previous grant floor across restart.
		restarted = selfhost.New(database, strings.Repeat("s", 32))
		if _, e = restarted.SetPool(ctx, account, "relay", selfhost.Policy{Mode: "mixed", InstallationIDs: []string{installed.InstallationID}}); e != nil {
			t.Fatal(e)
		}
		var after int64
		if e = database.SQL().QueryRowContext(ctx, `SELECT relay_revocation_generation FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, account, endpoint).Scan(&after); e != nil || after <= floor {
			t.Fatalf("restored pool reset old-token fence: %d %v", after, e)
		}

	})

	if mainBinary := os.Getenv("PAPERBOAT_SELFHOST_RELAY_BINARY"); mainBinary != "" {
		t.Run("actual main starts observes and fences removed credential", func(t *testing.T) {
			dir := t.TempDir()
			write := func(name string, b []byte) string {
				t.Helper()
				p := filepath.Join(dir, name)
				if e := os.WriteFile(p, b, 0600); e != nil {
					t.Fatal(e)
				}
				return p
			}
			cert := api.TLS.Certificates[0]
			certPath := write("tls.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
			privateDER, e := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
			if e != nil {
				t.Fatal(e)
			}
			privatePath := write("tls-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
			jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "fixture", "x": base64.RawURLEncoding.EncodeToString(public)}}})
			jwksPath := write("jwks.json", jwks)
			credentialPath := write("credential", []byte(installed.RuntimeCredential))
			var generation int64
			if e = database.SQL().QueryRowContext(ctx, `SELECT node_generation FROM paperboat.control_tunnel_nodes WHERE id=$1`, installed.NodeID).Scan(&generation); e != nil {
				t.Fatal(e)
			}
			processCtx, stop := context.WithCancel(ctx)
			defer stop()
			process := exec.CommandContext(processCtx, mainBinary, "-listen", "127.0.0.1:0", "-wss-listen", "127.0.0.1:0", "-tls-cert", certPath, "-tls-key", privatePath, "-jwks", jwksPath, "-issuer", "test", "-node-id", installed.NodeID, "-node-generation", strconv.FormatInt(generation, 10), "-control-url", api.URL, "-control-ca", certPath, "-control-credential-file", credentialPath, "-node-state", filepath.Join(dir, "node-state"))
			if e = process.Start(); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			exited := false
			go func() { done <- process.Wait() }()
			defer func() {
				if exited {
					return
				}
				stop()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("relay process did not stop")
				}
			}()
			ready := false
			deadline := time.Now().Add(12 * time.Second)
			for time.Now().Before(deadline) {
				var current int64
				if e = database.SQL().QueryRowContext(ctx, `SELECT ready,node_generation FROM paperboat.control_tunnel_nodes WHERE id=$1`, installed.NodeID).Scan(&ready, &current); e != nil {
					t.Fatal(e)
				}
				ready = ready && current > generation
				if ready {
					break
				}
				select {
				case err := <-done:
					exited = true
					t.Fatalf("relay exited before ready: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
			}
			if !ready {
				t.Fatal("relay main never observed ready")
			}
			if e = restarted.RemoveScoped(ctx, account, installed.InstallationID, false); e != nil {
				t.Fatal(e)
			}
			select {
			case <-done:
				exited = true
			case <-time.After(8 * time.Second):
				t.Fatal("removed runtime credential did not stop relay")
			}
		})
	}
}

func sha256sumRuntime(value string) []byte { digest := sha256.Sum256([]byte(value)); return digest[:] }
