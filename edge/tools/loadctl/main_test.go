package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/tlscert"
	"github.com/pinksaucepasta/paperboat/shared/tlscertwire"
)

// The wildcard certificate loadctl distributes must pass the edge's own registry checks and serve a preview name.
func TestDistributedWildcardCertificateIsAcceptedByTheEdgeReceiver(t *testing.T) {
	dir := t.TempDir()
	if err := initFiles([]string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	credential, err := os.ReadFile(filepath.Join(dir, "credential"))
	if err != nil {
		t.Fatal(err)
	}
	c := &controller{dir: dir, credential: string(credential), acked: map[string]bool{}}
	c.wildcards = []struct{ id, name string }{{"preview", "preview.example.test"}}
	c.registration = &control.NodeRegistration{NodeID: "edge_load", ProcessEpoch: "epoch_load_1"}
	recorder := httptest.NewRecorder()
	c.distribution(recorder)
	var page tlscertwire.DistributionPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil || len(page.Messages) != 2 {
		t.Fatalf("page %+v %v", page, err)
	}
	authenticator, err := control.NewHMACDistributionAuthenticator(string(credential))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tlscert.NewRegistry(tlscert.Config{})
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := tlscert.NewDistributionReceiver(tlscert.ReceiverConfig{Registry: registry, NodeID: "edge_load", ProcessEpoch: "epoch_load_1", Authenticator: authenticator})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page.Messages {
		fingerprint, _ := base64.RawURLEncoding.DecodeString(m.Fingerprint)
		envelope := tlscert.DistributionEnvelope{Action: m.Action, CertificateID: m.CertificateID, IssuedAt: m.IssuedAt, ExpiresAt: m.ExpiresAt, Proof: []byte(m.Proof),
			Binding: tlscert.Binding{AccountID: m.AccountID, DomainID: m.DomainID, TargetKind: tlscert.TargetKind(m.TargetKind), Hostname: m.Hostname,
				DomainGeneration: m.DomainGeneration, CertificateGeneration: m.CertificateGeneration, EdgeNodeID: m.EdgeNodeID, EdgeProcessEpoch: m.EdgeProcessEpoch, AssignmentGeneration: m.AssignmentGeneration},
			Bundle: tlscert.Bundle{CertificatePEM: m.CertificatePEM, PrivateKeyPEM: m.PrivateKeyPEM, Issuer: m.Issuer, NotBefore: m.NotBefore, NotAfter: m.CertificateExpiresAt}}
		copy(envelope.Fingerprint[:], fingerprint)
		if m.Action == "stage" {
			err = receiver.StageAndReady(context.Background(), envelope)
		} else {
			err = receiver.Activate(context.Background(), envelope)
		}
		if err != nil {
			t.Fatalf("%s: %v", m.Action, err)
		}
	}
	if _, err := registry.GetCertificate(&tls.ClientHelloInfo{ServerName: "ra0.preview.example.test"}); err != nil {
		t.Fatalf("no certificate for a preview name: %v", err)
	}
}
