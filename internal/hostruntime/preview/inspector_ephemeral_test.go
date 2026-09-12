package preview

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

// TestInspectorEphemeralCaptureAndReplay proves one shared daemon inspector
// implementation carries ephemeral browser captures, authorized viewing and
// deliberate replay: a real private preview request is captured sanitized,
// listed without secrets, replayed byte-exact to the same origin, audited
// without URL values, deduplicated by idempotency key, and purged when the
// lease's carrier ends. Durable-mode capture/replay shares the same store,
// registry, service and replay path via tunnelmanager's choke point.
func TestInspectorEphemeralCaptureAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var hits atomic.Int32
	var methods, uris, auths [4]string
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		index := int(hits.Add(1)) - 1
		if index < len(methods) {
			methods[index], uris[index], auths[index] = request.Method, request.URL.RequestURI(), request.Header.Get("Authorization")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"ok":true,"token":"zzz"}`)
	}))
	defer origin.Close()

	store := inspector.NewStore()
	registry := inspector.NewRegistry()
	grantNow := time.Now().UTC()
	grantDecision := func(principal, action string) inspectorapi.Decision {
		return inspectorapi.Decision{Principal: principal, Owner: "owner_01", ResourceKind: "preview", ResourceID: "preview-inspector", RouteID: "preview-inspector", ResourceGeneration: 1, RouteGeneration: 1, TargetGeneration: 1, IssuedAt: grantNow, ExpiresAt: grantNow.Add(time.Minute)}
	}
	grants := map[string]inspectorapi.Decision{
		"grant_owner_inspect": grantDecision("owner_01", "inspect"),
		"grant_owner_replay":  grantDecision("owner_01", "replay"),
	}
	grantActions := map[string]string{"grant_owner_inspect": "inspect", "grant_owner_replay": "replay"}
	service, err := inspectorapi.New(inspectorapi.Config{Store: store, Registry: registry, Authorize: func(_ context.Context, grantToken, kind, resource, route, action string) (inspectorapi.Decision, error) {
		decision, ok := grants[grantToken]
		if !ok || grantActions[grantToken] != action || kind != "preview" || resource != "preview-inspector" || route != "preview-inspector" {
			return inspectorapi.Decision{}, inspectorapi.ErrDenied
		}
		return decision, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	daemon := httptest.NewServer(service)
	defer daemon.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	call := func(method, path string, body any, token string) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = strings.NewReader(string(raw))
		}
		callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer callCancel()
		request, err := http.NewRequestWithContext(callCtx, method, daemon.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			request.Header.Set("X-Paperboat-Inspector-Grant", token)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, payload
	}

	identity := testPreviewCarrierIdentity(1)
	pair := newPreviewCarrierPair(t, ctx, identity)
	defer pair.close()
	hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	lease := Lease{ID: "preview-inspector", AccountID: identity.AccountID, Generation: 1, AccessMode: "private", Endpoint: "https://browser.example.test", Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String()}, LeaseDeadline: time.Now().UTC().Add(time.Hour)}
	now := time.Now()
	// Production carrier routes are opaque server hashes (pvc_rte_...), never
	// the preview lease ID. Captures stay owner-addressed by lease ID while
	// hub routing uses the carrier route.
	decision := connectorprotocol.IngressDecision{Binding: connectorprotocol.IngressBinding{EnvironmentID: "env-browser", AccountID: identity.AccountID, TunnelID: identity.TunnelID, Lifecycle: "ephemeral", ResourceGeneration: 1, RouteID: "pvc_rte_test", RouteGeneration: 1, TargetID: "pvc_rte_test", TargetGeneration: 1, HostID: identity.HostID, InstallationGeneration: 1, Audience: lease.AccessMode, ConnectionMethod: "edge", Protocol: "http", Hostname: "browser.example.test", PathPrefix: "/", OriginScheme: lease.Target.Scheme, OriginAddress: lease.Target.Address, TLSVerification: "not_applicable", PublicationID: lease.ID, PublicationGeneration: 1}, DecisionID: "decision-inspector", PolicyGeneration: 1, EdgeNodeID: "edge-browser", EdgeProcessEpoch: "epoch-1234567890123456789012345678", ConnectorID: identity.ConnectorID, SessionID: identity.SessionID, ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation, AssignmentGeneration: 1, PrincipalID: "owner-01", GrantID: "grant-inspector", GrantGeneration: 1, Action: "view", IssuedAt: now, ExpiresAt: now.Add(10 * time.Second)}

	// Owner enables capture before traffic.
	if status, payload := call(http.MethodPut, "/v1/inspector/policy", map[string]any{"resource_kind": "preview", "resource_id": lease.ID, "route_id": lease.ID, "enabled": true, "capture_request_body": true, "capture_response_body": true, "capture_raw": true}, "grant_owner_replay"); status != http.StatusOK {
		t.Fatalf("enable = %d: %s", status, payload)
	}

	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: hub, Identity: identity, RouteID: "pvc_rte_test", BrowserRouteGeneration: 1, Inspector: store, Registry: registry, DialOrigin: func(ctx context.Context, target LeaseTarget) (io.ReadWriteCloser, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
	}, BrowserIngress: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return decision, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- carrier.Run(ctx, lease, func(Lease) error { close(ready); return nil }) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("carrier not ready")
	}

	stream, err := pair.edge.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open := testPreviewStreamOpen(identity, "pvc_rte_test", "request-inspector")
	open.Kind = "http_browser"
	if err := connectorprotocol.WriteStreamOpen(stream, open); err != nil {
		t.Fatal(err)
	}
	if err := connectorprotocol.WriteIngressDecision(stream, decision, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(stream, "GET /items?session=secret HTTP/1.1\r\nHost: browser.example.test\r\nAuthorization: Bearer client-secret\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	_ = stream.Close()
	if err != nil || string(body) != `{"ok":true,"token":"zzz"}` {
		t.Fatalf("client bytes altered: %q err=%v", body, err)
	}
	if hits.Load() != 1 || methods[0] != "GET" || uris[0] != "/items?session=secret" || auths[0] != "Bearer client-secret" {
		t.Fatalf("origin saw %+v %+v %+v hits=%d", methods, uris, auths, hits.Load())
	}

	// Authorized viewing shows the sanitized record without secrets.
	status, payload := call(http.MethodGet, "/v1/inspector/records?kind=preview&resource="+lease.ID+"&route="+lease.ID+"&limit=10", nil, "grant_owner_inspect")
	if status != http.StatusOK {
		t.Fatalf("list = %d: %s", status, payload)
	}
	var page struct {
		Records []struct {
			ID               string              `json:"id"`
			URL              string              `json:"url"`
			RequestHeaders   map[string][]string `json:"request_headers"`
			ResponseBody     string              `json:"response_body"`
			ReplayIneligible string              `json:"replay_ineligible"`
		} `json:"records"`
	}
	if err := json.Unmarshal(payload, &page); err != nil || len(page.Records) != 1 {
		t.Fatalf("page = %s err=%v", payload, err)
	}
	record := page.Records[0]
	if !strings.Contains(record.URL, "/items") || strings.Contains(record.URL, "secret") {
		t.Fatalf("sanitized URL wrong: %q", record.URL)
	}
	if got := record.RequestHeaders["Authorization"]; len(got) != 1 || got[0] != "[redacted]" {
		t.Fatalf("authorization leaked: %v", record.RequestHeaders)
	}
	if strings.Contains(record.ResponseBody, "zzz") || !strings.Contains(record.ResponseBody, "ok") {
		t.Fatalf("response body wrong: %q", record.ResponseBody)
	}
	if record.ReplayIneligible != "" {
		t.Fatalf("complete raw capture must be replayable: %+v", record)
	}
	if status, _ := call(http.MethodGet, "/v1/inspector/records?kind=preview&resource="+lease.ID+"&route="+lease.ID, nil, "wrong-token"); status != http.StatusForbidden {
		t.Fatalf("wrong token list = %d, want 403", status)
	}
	if status, _ := call(http.MethodGet, "/v1/inspector/records?kind=preview&resource="+lease.ID+"&route="+lease.ID, nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("missing grant list = %d, want 401", status)
	}

	// Deliberate replay reuses exact bytes to the same origin and is audited.
	status, payload = call(http.MethodPost, "/v1/inspector/replay", map[string]any{"resource_kind": "preview", "resource_id": lease.ID, "route_id": lease.ID, "capture_id": record.ID, "idempotency_key": "replay_ephemeral_01"}, "grant_owner_replay")
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, payload)
	}
	var replay struct {
		OperationID    string `json:"operation_id"`
		SideEffects    string `json:"side_effects"`
		ResponseStatus int    `json:"response_status"`
		ReplayRecordID string `json:"replay_record_id"`
	}
	if err := json.Unmarshal(payload, &replay); err != nil || replay.OperationID == "" || replay.SideEffects == "" || replay.ResponseStatus != 200 || replay.ReplayRecordID == "" {
		t.Fatalf("replay result = %s err=%v", payload, err)
	}
	if hits.Load() != 2 || methods[1] != "GET" || uris[1] != "/items?session=secret" || auths[1] != "Bearer client-secret" {
		t.Fatalf("replay did not preserve bytes: %+v %+v %+v hits=%d", methods, uris, auths, hits.Load())
	}
	status, payload = call(http.MethodPost, "/v1/inspector/replay", map[string]any{"resource_kind": "preview", "resource_id": lease.ID, "route_id": lease.ID, "capture_id": record.ID, "idempotency_key": "replay_ephemeral_01"}, "grant_owner_replay")
	var duplicate struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(payload, &duplicate); err != nil || duplicate.OperationID != replay.OperationID || hits.Load() != 2 {
		t.Fatalf("duplicate replay opened a second request: %s hits=%d", payload, hits.Load())
	}
	status, payload = call(http.MethodGet, "/v1/inspector/audit?kind=preview&resource="+lease.ID+"&route="+lease.ID+"&limit=10", nil, "grant_owner_inspect")
	if status != http.StatusOK || !strings.Contains(string(payload), replay.OperationID) {
		t.Fatalf("audit = %d %s", status, payload)
	}
	if strings.Contains(string(payload), "/items") {
		t.Fatalf("audit leaked URL: %s", payload)
	}

	// Ending the lease's carrier purges retained captures immediately.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("carrier did not stop")
	}
	_ = carrier.Close(context.Background())
	status, _ = call(http.MethodGet, "/v1/inspector/records?kind=preview&resource="+lease.ID+"&route="+lease.ID+"&limit=10", nil, "grant_owner_inspect")
	if status != http.StatusUnauthorized && status != http.StatusNotFound && status != http.StatusGone {
		t.Fatalf("purged list = %d, want denial", status)
	}
	status, _ = call(http.MethodPost, "/v1/inspector/replay", map[string]any{"resource_kind": "preview", "resource_id": lease.ID, "route_id": lease.ID, "capture_id": record.ID, "idempotency_key": "replay_ephemeral_02"}, "grant_owner_replay")
	if status == http.StatusOK {
		t.Fatal("replay after purge must not succeed")
	}
}
