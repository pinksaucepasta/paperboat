package controlplane

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

func testMintProvider(t *testing.T, active string, ids ...string) *mint.Provider {
	t.Helper()
	keys := make([]mint.Key, 0, len(ids))
	for _, id := range ids {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, mint.Key{ID: id, PrivateKey: private})
	}
	provider, err := mint.New(keys, active, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// A node refreshes the mint trust document over the authenticated edge-control channel: it lists a key
// published before the old one is withdrawn, then names the withdrawn one, and an unauthenticated caller gets none.
func TestEdgeServesMintTrustDocumentThroughRotation(t *testing.T) {
	const credential = "managed-control-credential-012345678901"
	signer := testMintProvider(t, "new", "old", "new")
	service := NewEdgeService(nil, credential)
	service.SetCredentialIssuer(signer, "https://issuer.example.test", "")
	fetch := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/trust/jwks", nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		return response
	}
	if got := fetch("").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", got)
	}
	var document struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
		Withdrawn []string `json:"withdrawn_key_ids"`
	}
	response := fetch(credential)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &document) != nil || len(document.Keys) != 2 || len(document.Withdrawn) != 0 {
		t.Fatalf("overlap document = %d %s", response.Code, response.Body.String())
	}
	if err := signer.Withdraw([]string{"old"}); err != nil {
		t.Fatal(err)
	}
	document.Keys, document.Withdrawn = nil, nil
	response = fetch(credential)
	if json.Unmarshal(response.Body.Bytes(), &document) != nil || len(document.Keys) != 1 || document.Keys[0].Kid != "new" || len(document.Withdrawn) != 1 || document.Withdrawn[0] != "old" {
		t.Fatalf("withdrawn document = %s", response.Body.String())
	}
	if signer.Withdraw([]string{"new"}) == nil {
		t.Fatal("active key withdrawn")
	}
	unconfigured := NewEdgeService(nil, credential)
	request := httptest.NewRequest(http.MethodGet, "/v1/trust/jwks", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	response = httptest.NewRecorder()
	unconfigured.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d", response.Code)
	}
}

// An operator revocation names a key the mint publishes and does not sign with: an unknown, overlong or
// active kid is refused before it can reach a served trust document (and make every client refuse it).
func TestRevokeSigningKeyRefusesUnknownOverlongAndActiveKeys(t *testing.T) {
	service := NewEdgeService(nil, "managed-control-credential-012345678901")
	service.SetCredentialIssuer(testMintProvider(t, "new", "old", "new"), "https://issuer.example.test", "")
	for _, kid := range []string{"unknown", strings.Repeat("k", 200), "new"} {
		if err := service.RevokeSigningKey(t.Context(), "opr_1", "op-"+kid[:3], kid, "compromise", time.Now()); !errors.Is(err, ErrInvalidUsageReport) {
			t.Errorf("RevokeSigningKey(%.12q) = %v, want ErrInvalidUsageReport", kid, err)
		}
	}
}
