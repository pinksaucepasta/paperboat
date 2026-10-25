package control

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const http01FixtureToken = "tok_fixture-01"

// TestIngressSnapshotCarriesHTTP01ResponsesToEdgeHandler proves the edge
// path end to end without a CA: the server's snapshot is decoded, the
// response is served only for its hostname, and a withdrawn token is gone
// after the next complete snapshot.
func TestIngressSnapshotCarriesHTTP01ResponsesToEdgeHandler(t *testing.T) {
	decision := ingressAuthorityTestDecision(time.Now().UTC())
	response := connectorprotocol.HTTP01Response{Hostname: "app.customer.test", Token: http01FixtureToken, KeyAuthorization: http01FixtureToken + ".thumb"}
	var served []connectorprotocol.HTTP01Response
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"complete": true, "decisions": []connectorprotocol.IngressDecision{decision}, "quota_denied": []any{}, "http01_responses": served})
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authority := &IngressAuthority{Client: client, NodeID: decision.EdgeNodeID, ProcessEpoch: decision.EdgeProcessEpoch}
	handler := authority.HTTP01Handler(http.NotFoundHandler())
	get := func(host, path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	served = []connectorprotocol.HTTP01Response{response}
	if _, err := authority.snapshot(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	ok := get("App.customer.test:80", "/.well-known/acme-challenge/"+http01FixtureToken)
	if ok.Code != http.StatusOK {
		t.Fatalf("published response not served: %d", ok.Code)
	}
	if body, _ := io.ReadAll(ok.Body); string(body) != response.KeyAuthorization {
		t.Fatalf("served body is not the issued key authorization")
	}
	if got := get("other.customer.test", "/.well-known/acme-challenge/"+http01FixtureToken); got.Code != http.StatusNotFound {
		t.Fatalf("response served for a different hostname: %d", got.Code)
	}
	if got := get("app.customer.test", "/.well-known/acme-challenge/tok_unknown"); got.Code != http.StatusNotFound {
		t.Fatalf("unknown token answered: %d", got.Code)
	}

	served = nil
	if _, err := authority.snapshot(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if got := get("app.customer.test", "/.well-known/acme-challenge/"+http01FixtureToken); got.Code != http.StatusNotFound {
		t.Fatalf("withdrawn response still served: %d", got.Code)
	}
}

func TestHTTP01HandlerPassesOtherPathsToRedirect(t *testing.T) {
	authority := &IngressAuthority{}
	handler := authority.HTTP01Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/app", nil))
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("non-ACME path did not reach the redirect handler: %d", recorder.Code)
	}
}
