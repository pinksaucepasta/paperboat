package selfhost

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testInstallation(t *testing.T, both bool) (string, string, *http.Client, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{Name: "native-test", EndpointHost: "127.0.0.1", Listen: "127.0.0.1:18443", Components: []Component{{Capability: "relay", TCPPort: 27445, QUICPort: 27446, Region: "test", FailureDomain: "test-host", CapacityLimit: 256}}}
	if both {
		c.Components = append(c.Components, Component{Capability: "tunnel", TCPPort: 27443, QUICPort: 27444, Region: "test", FailureDomain: "test-host", CapacityLimit: 128})
	}
	if err := Initialize(dir, c); err != nil {
		t.Fatal(err)
	}
	code, _, _, err := CreateCode(dir)
	if err != nil {
		t.Fatal(err)
	}
	pin, secret, err := ParseCode(code)
	if err != nil {
		t.Fatal(err)
	}
	client, err := PinnedClient(pin)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := TLSCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(Handler(dir, nil))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	return dir, encoding.EncodeToString(secret), client, server
}
func post(t *testing.T, client *http.Client, target, secret string, v any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(v)
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}
func testClaim(both bool) ClaimRequest {
	r := ClaimRequest{ClaimID: "claim-native", ScopeType: "team", ScopeID: "team-native", ControlURL: "https://control.example.test", JWKS: json.RawMessage(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"key-native","x":"dGVzdA"}]}`), Revocations: json.RawMessage(`{"version":1,"revocations":[]}`), Components: []RegisteredComponent{{Capability: "relay", Registration: Registration{InstallationID: "relay-native", NodeID: "relay-node", RuntimeCredential: "runtime-test-credential-12345678901234567890", NodeGeneration: 1}}}}
	if both {
		r.Components = append(r.Components, RegisteredComponent{Capability: "tunnel", Registration: Registration{InstallationID: "tunnel-native", NodeID: "tunnel-node", RuntimeCredential: "runtime-test-credential-09876543210987654321", NodeGeneration: 1, UsageKeyID: "usage-native", EdgePool: "edge-native"}})
	}
	return r
}
func TestActualTLSClaimSingleUseRestartAndRuntimeFiles(t *testing.T) {
	dir, secret, client, server := testInstallation(t, true)
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	status, body := post(t, client, server.URL+"/v1/selfhost/inspect", secret, InspectRequest{Nonce: encoding.EncodeToString(nonce)})
	if status != 200 {
		t.Fatalf("inspect status %d", status)
	}
	var inspection Inspection
	if err := json.Unmarshal(body, &inspection); err != nil {
		t.Fatal(err)
	}
	if err := VerifyInspection(inspection); err != nil {
		t.Fatal(err)
	}
	if len(inspection.Components) != 2 || inspection.Components[1].UsagePublicKey == "" || inspection.InfrastructureTLSCertificate == "" {
		t.Fatal("inspection omitted identity")
	}
	inspection.Name = "tampered"
	if VerifyInspection(inspection) == nil {
		t.Fatal("accepted altered identity")
	}
	wrong := make([]byte, 32)
	status, _ = post(t, client, server.URL+"/v1/selfhost/claim", encoding.EncodeToString(wrong), testClaim(true))
	if status != 401 {
		t.Fatalf("wrong secret status %d", status)
	}
	status, body = post(t, client, server.URL+"/v1/selfhost/claim", secret, testClaim(true))
	if status != 200 {
		t.Fatalf("claim status %d", status)
	}
	var receipt Receipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Components) != 2 {
		t.Fatal("claim omitted component")
	}
	for _, name := range []string{"selfhost.json", "tls.key", "relay/runtime.credential", "relay/runtime.args", "tunnel/runtime.credential", "tunnel/deployment.json", "tunnel/usage.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("protected file %s unavailable or insecure", name)
		}
	}
	var deployment map[string]any
	b, err := os.ReadFile(filepath.Join(dir, "tunnel", "deployment.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment["self_hosted"] != true || deployment["preview_base_domain"] != "" || deployment["public_https_listen_address"] != "127.0.0.1:443" || deployment["carrier_tcp_listen_address"] != "127.0.0.1:27443" {
		t.Fatal("unconfigured tunnel domains were synthesized")
	}
	server.Config.Handler = Handler(dir, nil) // Rebuild handler with no memory from the first claim.
	status, again := post(t, client, server.URL+"/v1/selfhost/claim", secret, testClaim(true))
	if status != 200 || !bytes.Equal(body, again) {
		t.Fatal("same claim was not idempotent after restart")
	}
	other := testClaim(true)
	other.ScopeID = "other-team"
	status, _ = post(t, client, server.URL+"/v1/selfhost/claim", secret, other)
	if status != 409 {
		t.Fatalf("competing claim status %d", status)
	}
	status, _ = post(t, client, server.URL+"/v1/selfhost/inspect", secret, InspectRequest{Nonce: encoding.EncodeToString(nonce)})
	if status != 200 {
		t.Fatalf("consumed code recovery inspect status %d", status)
	}
	if _, _, _, err := CreateCode(dir); err == nil {
		t.Fatal("created a second ownership code for claimed installation")
	}
}
func TestActualTLSExpiryAndDurablePartialClaimRecovery(t *testing.T) {
	dir, secret, client, server := testInstallation(t, false)
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.ExpiresAt = time.Now().Add(-time.Second).Unix()
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	status, _ := post(t, client, server.URL+"/v1/selfhost/claim", secret, testClaim(false))
	if status != 410 {
		t.Fatalf("expired code status %d", status)
	}
	s.ExpiresAt = time.Now().Add(time.Minute).Unix()
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "relay"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _ = post(t, client, server.URL+"/v1/selfhost/claim", secret, testClaim(false))
	if status != 503 {
		t.Fatalf("partial claim status %d", status)
	}
	s, err = load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Claim == nil || s.RuntimeReady {
		t.Fatal("partial claim not durably fenced")
	}
	s.ExpiresAt = time.Now().Add(-time.Second).Unix()
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "relay")); err != nil {
		t.Fatal(err)
	}
	status, _ = post(t, client, server.URL+"/v1/selfhost/claim", secret, testClaim(false))
	if status != 200 {
		t.Fatalf("pending identical claim recovery after expiry status %d", status)
	}
}
func TestActualTLSConcurrentClaimFencesCompetingScope(t *testing.T) {
	dir, secret, client, server := testInstallation(t, false)
	_ = dir
	var wg sync.WaitGroup
	statuses := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := testClaim(false)
			if i%2 == 1 {
				req.ScopeID = "other-team"
			}
			b, _ := json.Marshal(req)
			r, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/selfhost/claim", bytes.NewReader(b))
			r.Header.Set("Authorization", "Bearer "+secret)
			resp, err := client.Do(r)
			if err != nil {
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}(i)
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		switch status {
		case 200:
			success++
		case 409, 503:
		default:
			t.Fatalf("concurrent claim status %d", status)
		}
	}
	if success == 0 {
		t.Fatal("no concurrent claim succeeded")
	}
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.RuntimeReady || s.Claim == nil {
		t.Fatal("concurrent claim lost durable state")
	}
}
func TestActualTLSPinRejectsWrongInstallation(t *testing.T) {
	_, secret, _, server := testInstallation(t, false)
	pin := make([]byte, 32)
	client, err := PinnedClient(pin)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/selfhost/inspect", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+secret)
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("wrong TLS identity accepted")
	}
}
func TestServeCancellationReleasesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	if err := Initialize(dir, Config{Name: "cancel", EndpointHost: "127.0.0.1", Listen: address, Components: []Component{{Capability: "relay", TCPPort: 27445, QUICPort: 27446, Region: "test", FailureDomain: "test", CapacityLimit: 1}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, nil) }()
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("claim listener did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatal("claim listener leaked")
	}
	listener.Close()
}
