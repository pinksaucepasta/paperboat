package tunnelcert

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
)

const edge01Bearer = "edge-01-runtime-credential-0123456789012345678901"

// testNodeBearers binds each runtime bearer to exactly one node, like the self-host credential store.
type testNodeBearers map[string]string

func (b testNodeBearers) RuntimeNode(_ context.Context, bearer string) (string, error) {
	if node, ok := b[bearer]; ok {
		return node, nil
	}
	return "", nodeauth.ErrNotFound
}
func (testNodeBearers) RuntimeCredential(context.Context, string) (string, error) {
	return "", nodeauth.ErrNotFound
}
func (testNodeBearers) IsSelfHosted(context.Context, string) (bool, error) { return false, nil }

// A node's queue is reachable only with that node's own runtime credential: the shared
// edge-control credential is refused, and a node header naming another node is ignored.
func TestDistributionQueueIsScopedToTheCallersOwnCredential(t *testing.T) {
	const shared = "distribution-credential-012345678901234567890123456789"
	const edge02Bearer = "edge-02-runtime-credential-0123456789012345678901"
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		NodeCredentials: testNodeBearers{edge01Bearer: "edge_01", edge02Bearer: "edge_02"},
		Credential:      []byte(shared), Now: func() time.Time { return now },
		// The identity proof is trusted here so only the credential binding decides.
		Identity: DistributionNodeIdentityResolverFunc(func(_ context.Context, r *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: r.Header.Get("X-Paperboat-Edge-Node-ID"), ProcessEpoch: "epoch_0002"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DistributionRequest{Certificate: StoredCertificate{
		ID: "certificate_02", AccountID: "account_02", TunnelID: "tunnel_02", DomainID: "domain_02", Hostname: "b.example.test",
		DomainGeneration: 1, CertificateGeneration: 1, Fingerprint: [32]byte{2}, Issuer: "test", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(24 * time.Hour),
	}, Target: DistributionTarget{NodeID: "edge_02", ProcessEpoch: "epoch_0002", Generation: 1}}
	if _, err := hub.enqueue(context.Background(), "stage", request); err != nil {
		t.Fatal(err)
	}
	pull := func(bearer, headerNode string) int {
		body, _ := json.Marshal(distributionPull{EdgeNodeID: "edge_02", EdgeProcessEpoch: "epoch_0002", Limit: 1})
		r := httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Paperboat-Edge-Node-ID", headerNode)
		w := httptest.NewRecorder()
		hub.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if code := pull(shared, "edge_02"); code != http.StatusUnauthorized {
		t.Fatalf("shared credential pull = %d, want 401", code)
	}
	if code := pull(edge01Bearer, "edge_02"); code != http.StatusUnauthorized {
		t.Fatalf("node A credential with node B header pull = %d, want 401", code)
	}
	if code := pull("unknown-runtime-credential-01234567890123456789", "edge_02"); code != http.StatusUnauthorized {
		t.Fatalf("unknown credential pull = %d, want 401", code)
	}
	if code := pull(edge02Bearer, "edge_02"); code != http.StatusOK {
		t.Fatalf("node B own pull = %d, want 200", code)
	}
}
