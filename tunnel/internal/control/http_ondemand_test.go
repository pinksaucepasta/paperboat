package control

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The server holds an on-demand certificate request while an issuance already in flight
// finishes; that wait outlives the control timeout, every other request does not.
func TestOnDemandCertificateRequestOutlivesTheControlTimeout(t *testing.T) {
	const slow = 1500 * time.Millisecond
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(slow)
		if r.URL.Path == CertificateDistributionRequestPath {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"complete":true,"version":"v1","routes":[]}`)
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: testControlCredential, Timeout: time.Second, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(nil)
	signer, err := NewEd25519DistributionRequestSigner(DistributionProofSignerConfig{PrivateKey: key, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
	if err != nil || client.SetDistributionRequestSigner(signer) != nil {
		t.Fatal(err)
	}
	distribution, err := NewCertificateDistributionClient(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := distribution.RequestCertificate(context.Background(), "edge_1", "edge_epoch_1", "app.example.test"); err != nil {
		t.Fatalf("on-demand certificate request cut by the control timeout: %v", err)
	}
	if _, err := client.DesiredRoutes(context.Background(), "edge"); err == nil {
		t.Fatal("an ordinary control request outlived the control timeout")
	}
}
