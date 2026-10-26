package controlplane_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/privateaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// Every control response comes from the production composition. The fixture
// changes only listener addresses to avoid binding public/shared workspace ports.
func TestSelfhostTunnelOperatorProcessOnPostgres(t *testing.T) {
	binary, dsn := os.Getenv("PAPERBOAT_SELFHOST_TUNNEL_BINARY"), os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if binary == "" || dsn == "" {
		t.Skip("requires prebuilt tunnel binary and isolated PostgreSQL")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	database, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if e = db.Migrate(ctx, database); e != nil {
		t.Fatal(e)
	}
	secret := strings.Repeat("e", 32)
	s := selfhost.New(database, secret)
	managed := strings.Repeat("managed", 8)
	edge := controlplane.NewEdgeService(database, managed)
	edge.SetNodeCredentials(s)
	signer, e := mint.NewEphemeral(time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	keys, e := tunnelcert.NewSQLDistributionNodePublicKeyLookup(database, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	identity, e := tunnelcert.NewSignedDistributionNodeIdentityResolver(keys, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	distribution, e := tunnelcert.NewDistributionHub(tunnelcert.DistributionHubConfig{Credential: []byte(managed), NodeCredentials: s, Identity: identity})
	if e != nil {
		t.Fatal(e)
	}
	edge.SetCertificateDistribution(distribution.Handler())
	verifier, e := previewattachment.NewDBPreviewEdgeRequestVerifier(database, managed)
	if e != nil {
		t.Fatal(e)
	}
	verifier.SetNodeCredentials(s)
	enrollment := controlplane.NewEnrollmentService(database, signer, audit.NewWriter(database), "https://control.example.test", secret)
	accessors, e := privateaccess.NewAccessorRepository(database)
	if e != nil {
		t.Fatal(e)
	}
	accessorHandler, e := privateaccess.NewAccessorHTTPHandler(accessors, enrollment, privateaccess.PreviewEdgeVerifierAdapter{Verifier: verifier})
	if e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /.well-known/jwks.json", signer)
	mux.Handle("POST "+privateaccess.EdgeAdmissionsPath, accessorHandler)
	mux.Handle("/", edge.Handler())
	api := httptest.NewTLSServer(mux)
	defer api.Close()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if e := os.WriteFile(p, b, 0600); e != nil {
			t.Fatal(e)
		}
		return p
	}
	caPath := write("control-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.TLS.Certificates[0].Certificate[0]}))
	host := "edge.example.test"
	publicHost := os.Getenv("PAPERBOAT_SELFHOST_PUBLIC_HOST")
	if publicHost != "" {
		host = publicHost
	}
	public, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if e != nil {
		t.Fatal(e)
	}
	certPath := write("infra.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	privateDER, e := x509.MarshalPKCS8PrivateKey(private)
	if e != nil {
		t.Fatal(e)
	}
	keyPath := write("infra-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	if publicHost != "" {
		certPath, keyPath = os.Getenv("PAPERBOAT_SELFHOST_PUBLIC_CERT"), os.Getenv("PAPERBOAT_SELFHOST_PUBLIC_KEY")
		if certPath == "" || keyPath == "" {
			t.Fatal("public verification requires exact public CA certificate and private key")
		}
	}
	reserve := func() string {
		t.Helper()
		listener, e := net.Listen("tcp4", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		address := listener.Addr().String()
		listener.Close()
		return address
	}
	tcpAddress, quicAddress, httpsAddress, httpAddress, privateAddress, healthAddress := reserve(), reserve(), reserve(), reserve(), reserve(), reserve()
	if publicHost != "" {
		httpsAddress = os.Getenv("PAPERBOAT_SELFHOST_PUBLIC_LISTEN")
		if httpsAddress == "" {
			t.Fatal("public verification requires explicit task-owned listener")
		}
	}
	randomName := make([]byte, 12)
	rand.Read(randomName)
	name := "tunnel-process-" + hex.EncodeToString(randomName)
	stateDir := filepath.Join(dir, "operator")
	// Cleanup uses only the unique installation name created by this fixture,
	// including setup interrupted after registration but before local export.
	t.Cleanup(func() {
		cleanup := context.Background()
		rows, e := database.SQL().QueryContext(cleanup, `SELECT id,node_id FROM paperboat.selfhost_installations WHERE name=$1`, name)
		if e != nil {
			t.Error(e)
			return
		}
		type item struct{ id, node string }
		items := []item{}
		for rows.Next() {
			var i item
			if e = rows.Scan(&i.id, &i.node); e != nil {
				t.Error(e)
				break
			}
			items = append(items, i)
		}
		rows.Close()
		for _, i := range items {
			for _, q := range []string{`DELETE FROM paperboat.selfhost_installations WHERE id=$1`} {
				if _, e = database.SQL().ExecContext(cleanup, q, i.id); e != nil {
					t.Error(e)
				}
			}
			if _, e = database.SQL().ExecContext(cleanup, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, i.node); e != nil {
				t.Error(e)
			}
		}
	})
	// Scoped SQL fixtures begin after enrollment. The pbh enrollment process is
	// exercised by the enrollment acceptance test; this test owns edge runtime.
	account := "tunnel_account_" + hex.EncodeToString(randomName)
	saved := selfhost.Registered{InstallationID: "tunnel_install_" + hex.EncodeToString(randomName), NodeID: "tunnel_node_" + hex.EncodeToString(randomName), RuntimeCredential: base64.RawURLEncoding.EncodeToString(private), NodeGeneration: 1, UsageKeyID: "tunnel_usage_" + hex.EncodeToString(randomName)}
	saved.EdgePool = saved.InstallationID
	if e = os.MkdirAll(stateDir, 0700); e != nil {
		t.Fatal(e)
	}
	oldCredential := saved.RuntimeCredential
	encrypted, e := secrets.Encrypt(secret, oldCredential)
	if e != nil {
		t.Fatal(e)
	}
	runtimeHash := sha256.Sum256([]byte(oldCredential))
	infrastructureCert, e := tls.LoadX509KeyPair(certPath, keyPath)
	if e != nil {
		t.Fatal(e)
	}
	infrastructureLeaf, e := x509.ParseCertificate(infrastructureCert.Certificate[0])
	if e != nil {
		t.Fatal(e)
	}
	infrastructurePin := sha256.Sum256(infrastructureLeaf.RawSubjectPublicKeyInfo)
	sqlExec := func(q string, args ...any) {
		t.Helper()
		if _, e := database.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sqlExec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	t.Cleanup(func() {
		database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	sqlExec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,region,failure_domain,roles,transports,capacity_limit,capacity_observed_at,registry_expires_at,tls_spki_sha256) VALUES($1,$2,'1.0','initial_fixture',$3,$4,$5,'registered','test','test-process',ARRAY['edge'],ARRAY['http2','http3'],32,now(),now()+interval '1 hour',$6)`, saved.NodeID, saved.EdgePool, host, fixturePort(t, tcpAddress), fixturePort(t, quicAddress), infrastructurePin[:])
	sqlExec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$4,$5,$6,'tunnel','account',$7,$1,true)`, saved.InstallationID, saved.NodeID, []byte(public), runtimeHash[:], encrypted, name, account)
	sqlExec(`INSERT INTO paperboat.control_usage_verification_keys(key_id,edge_node_id,public_key,not_before,expires_at) VALUES($1,$2,$3,now()-interval '1 minute',now()+interval '1 hour')`, saved.UsageKeyID, saved.NodeID, []byte(public))
	writeJSON := func(name string, value any) string {
		t.Helper()
		data, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		return write(name, data)
	}
	credentialPath := write("runtime.credential", []byte(oldCredential))
	jwks, e := signer.JWKSJSON()
	if e != nil {
		t.Fatal(e)
	}
	jwksPath := write("jwks.json", jwks)
	revocations, e := edge.Revocations(ctx)
	if e != nil {
		t.Fatal(e)
	}
	revocationsPath := writeJSON("revocations.json", revocations)
	usagePath := writeJSON("usage.key", map[string]string{"key_id": saved.UsageKeyID, "edge_node_id": saved.NodeID, "private_key": base64.RawURLEncoding.EncodeToString(private)})
	deploymentPath := writeJSON("deployment.json", map[string]any{"self_hosted": true, "browser_access_enabled": false, "browser_login_origin": "", "control_url": api.URL, "credential_issuer": api.URL, "control_credential_file": credentialPath, "control_ca_file": caPath, "jwks_file": jwksPath, "revocations_file": revocationsPath, "usage_signing_key_file": usagePath, "infrastructure_tls_cert_file": certPath, "infrastructure_tls_key_file": keyPath, "connector_advertise_host": host, "carrier_tcp_listen_address": tcpAddress, "carrier_quic_listen_address": quicAddress, "public_https_listen_address": httpsAddress, "private_https_listen_address": privateAddress, "public_http_listen_address": httpAddress, "preview_base_domain": "preview.example.test", "tunnel_base_domain": "tunnel.example.test", "runtime_base_domain": "runtime.example.test", "trusted_proxy_cidrs": []string{}, "public_routes": []any{}, "node_capacity": 32, "control_interval": int64(time.Second), "control_timeout": int64(5 * time.Second), "max_body_bytes": int64(50 << 20), "max_header_bytes": int64(32 << 10)})
	args := []string{"-node-id", saved.NodeID, "-edge-pool", saved.EdgePool, "-relay-id", saved.NodeID, "-relay-region", "test", "-relay-name", name, "-health-address", healthAddress, "-deployment-config", deploymentPath, "-state-path", filepath.Join(stateDir, "state.json")}
	logPath := filepath.Join(dir, "process.log")
	log, e := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer log.Close()
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	process := exec.CommandContext(processCtx, binary, args...)
	process.Stdout, process.Stderr = log, log
	if e = process.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	exited := false
	defer func() {
		if !exited {
			stop()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("edge process did not stop")
			}
		}
	}()
	failProcess := func(message string) {
		t.Helper()
		stop()
		if !exited {
			<-done
			exited = true
		}
		body, _ := os.ReadFile(logPath)
		t.Fatalf("%s\n%s", message, body)
	}
	healthClient := &http.Client{Timeout: time.Second}
	defer healthClient.CloseIdleConnections()
	ready := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		response, err := healthClient.Get("http://" + healthAddress + "/readyz")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		select {
		case <-done:
			exited = true
			failProcess("edge exited before ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		failProcess("generated edge never became ready")
	}
	var dbReady bool
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e = database.SQL().QueryRowContext(ctx, `SELECT ready AND state='ready' AND registry_expires_at>now() FROM paperboat.control_tunnel_nodes WHERE id=$1`, saved.NodeID).Scan(&dbReady)
		if e != nil {
			t.Fatal(e)
		}
		if dbReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !dbReady {
		t.Fatal("registered edge lease not ready")
	}
	request, e := http.NewRequestWithContext(ctx, "POST", api.URL+"/v1/nodes/heartbeat", bytes.NewBufferString(`{"node_id":"different_node"}`))
	if e != nil {
		t.Fatal(e)
	}
	request.Header.Set("Authorization", "Bearer "+oldCredential)
	request.Header.Set("Content-Type", "application/json")
	denied, e := api.Client().Do(request)
	if e != nil {
		t.Fatal(e)
	}
	denied.Body.Close()
	if denied.StatusCode != 403 && denied.StatusCode != 401 {
		t.Fatalf("other node credential accepted: %d", denied.StatusCode)
	}
	leaf, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if publicHost != "" {
		roots, e = x509.SystemCertPool()
		if e != nil {
			t.Fatal(e)
		}
		if e = s.VerifyIngress(ctx, saved.InstallationID); e != nil {
			t.Fatalf("real public ingress verification: %v", e)
		}
		t.Log("public DNS HTTPS443 exact certificate and production VerifyIngress passed")
	}
	dialer := &net.Dialer{Timeout: time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", httpsAddress)
	}}
	defer transport.CloseIdleConnections()
	ingressClient := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	challenge := make([]byte, 32)
	rand.Read(challenge)
	encoded := base64.RawURLEncoding.EncodeToString(challenge)
	response, e := ingressClient.Get("https://" + host + "/.well-known/paperboat-installation?challenge=" + encoded)
	if e != nil {
		t.Fatal(e)
	}
	var proof struct {
		NodeID    string `json:"node_id"`
		Challenge string `json:"challenge"`
		Proof     string `json:"proof"`
	}
	e = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&proof)
	response.Body.Close()
	mac := hmac.New(sha256.New, []byte(oldCredential))
	mac.Write([]byte("paperboat-installation/v1\n" + saved.NodeID + "\n" + encoded))
	signature, decodeErr := base64.RawURLEncoding.DecodeString(proof.Proof)
	if e != nil || decodeErr != nil || response.StatusCode != 200 || proof.NodeID != saved.NodeID || proof.Challenge != encoded || !hmac.Equal(signature, mac.Sum(nil)) {
		t.Fatal("infrastructure TLS challenge not bound to registered node")
	}
	wrong := transport.Clone()
	wrong.TLSClientConfig = transport.TLSClientConfig.Clone()
	wrong.TLSClientConfig.ServerName = "other.example.test"
	wrongClient := &http.Client{Transport: wrong, Timeout: 3 * time.Second}
	if result, err := wrongClient.Get("https://other.example.test/"); err == nil {
		result.Body.Close()
		t.Fatal("infrastructure certificate served for another hostname")
	}
	wrong.CloseIdleConnections()
	if e = s.RemoveScoped(ctx, account, saved.InstallationID, false); e != nil {
		t.Fatal(e)
	}
	request, e = http.NewRequestWithContext(ctx, "GET", api.URL+"/v1/trust/revocations", nil)
	if e != nil {
		t.Fatal(e)
	}
	request.Header.Set("Authorization", "Bearer "+oldCredential)
	denied, e = api.Client().Do(request)
	if e != nil {
		t.Fatal(e)
	}
	denied.Body.Close()
	if denied.StatusCode != 401 && denied.StatusCode != 403 {
		t.Fatal("old runtime credential retained control access")
	}
	unhealthy := false
	deadline = time.Now().Add(18 * time.Second)
	for time.Now().Before(deadline) {
		response, err := healthClient.Get("http://" + healthAddress + "/readyz")
		if err != nil {
			unhealthy = true
			break
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			unhealthy = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !unhealthy {
		failProcess("removed edge continued reporting ready")
	}
	// No resource is installed in this fixture. Public-DNS verification and
	// forwarding/active-stream expiry remain their separate integration gates.
	t.Log("scoped runtime fixtures, real edge registration, infrastructure SNI/HMAC, scoped denial and removal readiness verified; public DNS VerifyIngress exercised only with explicit public fixture configuration")
}

func fixturePort(t *testing.T, address string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
