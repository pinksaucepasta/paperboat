package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	endpointOnePageBody = `{"schema":"paperboat.runtime-carrier/v1","complete":true,"carriers":[],"cursor":"cursor_1"}`
	certificateOnePage  = `{"version":1,"complete":true,"messages":[]}`
)

// servePage answers every request with the same body, as a control plane with one page would.
func servePage(t *testing.T, body string) *HTTPClient {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestEndpointAdmissionsRefusesPagesOverTheCumulativeBudget(t *testing.T) {
	saved := endpointPagesMaximumBytes
	t.Cleanup(func() { endpointPagesMaximumBytes = saved })
	client := servePage(t, endpointOnePageBody)

	endpointPagesMaximumBytes = 1
	if _, err := client.EndpointAdmissions(context.Background(), "edge_1", "edge_epoch_1", "", 0); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("pages over the budget: got %v, want ErrControlUnavailable", err)
	}

	endpointPagesMaximumBytes = 1 << 20
	update, err := client.EndpointAdmissions(context.Background(), "edge_1", "edge_epoch_1", "", 0)
	if err != nil || update.Cursor != "cursor_1" {
		t.Fatalf("pages within the budget: update %+v, err %v", update, err)
	}
}

func TestCertificateDistributionPullRefusesPagesOverTheCumulativeBudget(t *testing.T) {
	saved := certificateDistributionPagesMaximumBytes
	t.Cleanup(func() { certificateDistributionPagesMaximumBytes = saved })
	httpClient := servePage(t, certificateOnePage)
	_, key, _ := ed25519.GenerateKey(nil)
	signer, err := NewEd25519DistributionRequestSigner(DistributionProofSignerConfig{PrivateKey: key, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1"})
	if err != nil || httpClient.SetDistributionRequestSigner(signer) != nil {
		t.Fatal(err)
	}
	client, err := NewCertificateDistributionClient(httpClient)
	if err != nil {
		t.Fatal(err)
	}

	certificateDistributionPagesMaximumBytes = 1
	if _, err := client.Pull(context.Background(), "edge_1", "edge_epoch_1"); !errors.Is(err, ErrCertificateDistributionInvalid) {
		t.Fatalf("pages over the budget: got %v, want ErrCertificateDistributionInvalid", err)
	}

	certificateDistributionPagesMaximumBytes = 1 << 20
	if _, err := client.Pull(context.Background(), "edge_1", "edge_epoch_1"); err != nil {
		t.Fatalf("pages within the budget: %v", err)
	}
}
