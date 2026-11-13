package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Requests the server refuses from an unready edge (on-demand certificates, browser access,
// private access grants) are held until NodeReady closes: before it nothing is sent and no
// control failure is recorded; after it the request goes out.
func TestUnreadyNodeSendsNoReadyGatedRequest(t *testing.T) {
	ready := make(chan struct{})
	var sent, failures atomic.Int32
	client, err := NewHTTPClient(HTTPConfig{BaseURL: "https://control.test", Credential: testControlCredential, Timeout: time.Second, NodeReady: ready, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent.Add(1)
		return response(http.StatusNoContent, ""), nil
	})}, ControlFailure: func(context.Context, string, error) { failures.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewEd25519DistributionRequestSigner(DistributionProofSignerConfig{PrivateKey: key, NodeID: "edge_01", ProcessEpoch: "epoch_0001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetDistributionRequestSigner(signer); err != nil {
		t.Fatal(err)
	}
	distribution, err := NewCertificateDistributionClient(client)
	if err != nil {
		t.Fatal(err)
	}
	certificates, err := NewOnDemandCertificateRequester(distribution, "edge_01", "epoch_0001")
	if err != nil {
		t.Fatal(err)
	}
	browser := &BrowserAccessClient{HTTP: client, NodeID: "edge_01", ProcessEpoch: "epoch_0001"}
	requests := map[string]func(context.Context) error{
		"certificate": func(ctx context.Context) error { return certificates.RequestCertificate(ctx, "app.example.test") },
		"browser":     func(ctx context.Context) error { _, err := browser.Begin(ctx, "app.example.test", "/"); return err },
	}
	for name, request := range requests {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := request(ctx)
		cancel()
		if !errors.Is(err, ErrNodeNotReady) {
			t.Fatalf("%s before ready: %v", name, err)
		}
	}
	if sent.Load() != 0 || failures.Load() != 0 {
		t.Fatalf("before ready: sent %d, control failures %d", sent.Load(), failures.Load())
	}
	close(ready)
	_ = requests["certificate"](context.Background())
	_ = requests["browser"](context.Background())
	if sent.Load() != 2 {
		t.Fatalf("after ready: sent %d requests, want 2", sent.Load())
	}
}
