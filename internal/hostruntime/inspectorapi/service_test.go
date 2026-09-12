package inspectorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

type fakeAuthorizer struct {
	decisions map[string]Decision
	err       error
	calls     int
}

func (f *fakeAuthorizer) authorize(_ context.Context, grantToken, kind, resource, route, action string) (Decision, error) {
	f.calls++
	if f.err != nil {
		return Decision{}, f.err
	}
	key := grantToken + "\x00" + kind + "\x00" + resource + "\x00" + route + "\x00" + action
	decision, ok := f.decisions[key]
	if !ok {
		return Decision{}, ErrDenied
	}
	return decision, nil
}

func testDecision(principal, kind, resource, route string, now time.Time) Decision {
	return Decision{
		Principal: principal, Owner: "owner_01",
		ResourceKind: kind, ResourceID: resource, RouteID: route,
		ResourceGeneration: 3, RouteGeneration: 3, TargetGeneration: 3,
		CredentialID: "iac_test", IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
}

func testService(t *testing.T, authorizer *fakeAuthorizer) (*Service, *inspector.Store, *inspector.Registry) {
	t.Helper()
	store := inspector.NewStore()
	registry := inspector.NewRegistry()
	service, err := New(Config{Store: store, Registry: registry, Authorize: authorizer.authorize})
	if err != nil {
		t.Fatal(err)
	}
	return service, store, registry
}

func testRequest(t *testing.T, method, target string, body any, grant string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, target, reader)
	request.RemoteAddr = "127.0.0.1:43210"
	if grant != "" {
		request.Header.Set(grantHeader, grant)
	}
	return request
}

func seedCapture(t *testing.T, store *inspector.Store, resourceID string, now time.Time) string {
	t.Helper()
	if err := store.SetPolicy(resourceID, inspector.ResourcePolicy{
		Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true, CaptureRaw: true,
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.TryBegin(resourceID)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("GET /api/items HTTP/1.1\r\nHost: origin.example\r\nContent-Type: application/json\r\n\r\n")
	completed := inspector.CompletedCapture{
		Method: "GET", RawURL: "https://app.example.test/api/items",
		RequestHeaders:  http.Header{"Content-Type": {"application/json"}},
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		ResponseBody:    []byte(`{"ok":true}`),
		ResponseStatus:  200, StartedAt: now, FinishedAt: now.Add(10 * time.Millisecond),
		RawRequest: raw, ResourceGeneration: 3, RouteGeneration: 3, TargetGeneration: 3,
	}
	record, err := store.Finish(pending, completed)
	if err != nil {
		t.Fatal(err)
	}
	return record.ID
}

func TestGrantTokenRequired(t *testing.T) {
	authorizer := &fakeAuthorizer{}
	service, _, _ := testService(t, authorizer)
	recorder := httptest.NewRecorder()
	request := testRequest(t, http.MethodGet, "/v1/inspector/records?resource=tunnel_01&kind=tunnel&route=tunnel_01", nil, "")
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing grant = %d, want 401", recorder.Code)
	}
	if authorizer.calls != 0 {
		t.Fatal("unauthenticated request reached the authorizer")
	}
}

func TestLoopbackEnforced(t *testing.T) {
	authorizer := &fakeAuthorizer{}
	service, _, _ := testService(t, authorizer)
	recorder := httptest.NewRecorder()
	request := testRequest(t, http.MethodGet, "/v1/inspector/records?resource=tunnel_01&kind=tunnel&route=tunnel_01", nil, "grant_owner")
	request.RemoteAddr = "192.0.2.1:43210"
	service.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("non-loopback = %d, want 401", recorder.Code)
	}
}

func TestInspectorsArePerPrincipal(t *testing.T) {
	now := time.Now().UTC()
	authorizer := &fakeAuthorizer{decisions: map[string]Decision{
		"grant_owner\x00tunnel\x00tun_01\x00rte_01\x00inspect": testDecision("owner_01", "tunnel", "tun_01", "rte_01", now),
		"grant_mate\x00tunnel\x00tun_01\x00rte_01\x00inspect":  testDecision("mate_01", "tunnel", "tun_01", "rte_01", now),
	}}
	service, store, _ := testService(t, authorizer)
	id := seedCapture(t, store, "rte_01", now)

	// Owner lists with their own grant.
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodGet, "/v1/inspector/records?resource=tun_01&route=rte_01&kind=tunnel&limit=10", nil, "grant_owner"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("owner list = %d: %s", recorder.Code, recorder.Body.String())
	}
	// Teammate lists with their own grant: same data, distinct principal.
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodGet, "/v1/inspector/records?resource=tun_01&route=rte_01&kind=tunnel&limit=10", nil, "grant_mate"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("mate list = %d: %s", recorder.Code, recorder.Body.String())
	}
	// Unknown grant is denied without touching the store.
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodGet, "/v1/inspector/records/"+id+"?resource=tun_01&route=rte_01&kind=tunnel", nil, "grant_stranger"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("stranger get = %d, want 403", recorder.Code)
	}
	// Inspect grant cannot drive replay: separate explicit action required.
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodPost, "/v1/inspector/replay", map[string]any{
		"resource_kind": "tunnel", "resource_id": "tun_01", "route_id": "rte_01", "capture_id": id, "idempotency_key": "key_01",
	}, "grant_mate"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("inspect-only replay = %d, want 403", recorder.Code)
	}
}

func TestPolicyAndPurgeRequireReplay(t *testing.T) {
	now := time.Now().UTC()
	authorizer := &fakeAuthorizer{decisions: map[string]Decision{
		"grant_inspect\x00tunnel\x00tun_01\x00rte_01\x00inspect": testDecision("mate_01", "tunnel", "tun_01", "rte_01", now),
		"grant_replay\x00tunnel\x00tun_01\x00rte_01\x00replay":   testDecision("mate_01", "tunnel", "tun_01", "rte_01", now),
	}}
	service, store, _ := testService(t, authorizer)
	id := seedCapture(t, store, "rte_01", now)

	policy := map[string]any{"resource_kind": "tunnel", "resource_id": "tun_01", "route_id": "rte_01", "enabled": false}
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodPut, "/v1/inspector/policy", policy, "grant_inspect"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("inspect-only policy = %d, want 403", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodPut, "/v1/inspector/policy", policy, "grant_replay"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay policy = %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodDelete, "/v1/inspector/records?resource=tun_01&route=rte_01&kind=tunnel", nil, "grant_inspect"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("inspect-only purge = %d, want 403", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodDelete, "/v1/inspector/records?resource=tun_01&route=rte_01&kind=tunnel", nil, "grant_replay"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay purge = %d", recorder.Code)
	}
	// Purge denied retrieval for everyone, including the purger.
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodGet, "/v1/inspector/records/"+id+"?resource=tun_01&route=rte_01&kind=tunnel", nil, "grant_replay"))
	if recorder.Code == http.StatusOK {
		t.Fatal("purged record still retrievable")
	}
}

func TestStaleDecisionDeniesWithConflict(t *testing.T) {
	now := time.Now().UTC()
	stale := testDecision("mate_01", "tunnel", "tun_01", "rte_01", now)
	stale.RouteGeneration = 2
	authorizer := &fakeAuthorizer{decisions: map[string]Decision{
		"grant_mate\x00tunnel\x00tun_01\x00rte_01\x00inspect": stale,
	}}
	service, store, _ := testService(t, authorizer)
	id := seedCapture(t, store, "rte_01", now)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodGet, "/v1/inspector/records/"+id+"?resource=tun_01&route=rte_01&kind=tunnel", nil, "grant_mate"))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale get = %d, want 409", recorder.Code)
	}
}

func TestReplayEndToEndAndAudit(t *testing.T) {
	now := time.Now().UTC()
	authorizer := &fakeAuthorizer{decisions: map[string]Decision{
		"grant_owner_replay\x00tunnel\x00tun_01\x00rte_01\x00replay":   testDecision("owner_01", "tunnel", "tun_01", "rte_01", now),
		"grant_owner_inspect\x00tunnel\x00tun_01\x00rte_01\x00inspect": testDecision("owner_01", "tunnel", "tun_01", "rte_01", now),
	}}
	service, store, registry := testService(t, authorizer)
	id := seedCapture(t, store, "rte_01", now)
	forward := func(ctx context.Context, method, uri string, header http.Header, body []byte) (int, http.Header, []byte, bool, error) {
		if method != "GET" || uri != "/api/items" {
			t.Errorf("replay = %s %s", method, uri)
		}
		return 200, http.Header{"Content-Type": {"application/json"}}, []byte(`{"ok":true,"n":1}`), false, nil
	}
	if err := registry.Register("rte_01", inspector.ReplayBinding{ResourceGeneration: 3, RouteGeneration: 3, TargetGeneration: 3, ExpiresAt: now.Add(5 * time.Minute), Forward: forward}); err != nil {
		t.Fatal(err)
	}
	_ = id
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodPost, "/v1/inspector/replay", map[string]any{
		"resource_kind": "tunnel", "resource_id": "tun_01", "route_id": "rte_01", "capture_id": id, "idempotency_key": "replay_http_01",
	}, "grant_owner_replay"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay = %d: %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.OperationID == "" {
		t.Fatalf("result = %s err=%v", recorder.Body.String(), err)
	}
	// Audit is per-resource and requires inspect; entries name the actor.
	recorder = httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodGet, "/v1/inspector/audit?resource=tun_01&route=rte_01&kind=tunnel&limit=10", nil, "grant_owner_inspect"))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), result.OperationID) || !strings.Contains(recorder.Body.String(), "owner_01") {
		t.Fatalf("audit = %d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "/api/items") {
		t.Fatalf("audit leaked URL: %s", recorder.Body.String())
	}
}

func TestReplayWithoutBindingIsGone(t *testing.T) {
	now := time.Now().UTC()
	authorizer := &fakeAuthorizer{decisions: map[string]Decision{
		"grant_owner_replay\x00tunnel\x00tun_01\x00rte_01\x00replay": testDecision("owner_01", "tunnel", "tun_01", "rte_01", now),
	}}
	service, store, _ := testService(t, authorizer)
	if err := store.SetPolicy("tun_01", inspector.ResourcePolicy{Enabled: true, CaptureRaw: true}); err != nil {
		t.Fatal(err)
	}
	id := seedCapture(t, store, "tun_01", now)
	_ = id
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodPost, "/v1/inspector/replay", map[string]any{
		"resource_kind": "tunnel", "resource_id": "tun_01", "route_id": "rte_01", "capture_id": id, "idempotency_key": "replay_http_02",
	}, "grant_owner_replay"))
	if recorder.Code != http.StatusGone {
		t.Fatalf("binding-less replay = %d, want 410", recorder.Code)
	}
}

func TestUpstreamFailureIsUnavailable(t *testing.T) {
	authorizer := &fakeAuthorizer{err: ErrUpstream}
	service, _, _ := testService(t, authorizer)
	recorder := httptest.NewRecorder()
	service.ServeHTTP(recorder, testRequest(t, http.MethodGet, "/v1/inspector/records?resource=tun_01&route=rte_01&kind=tunnel", nil, "grant_owner"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("upstream failure = %d, want 503", recorder.Code)
	}
}
