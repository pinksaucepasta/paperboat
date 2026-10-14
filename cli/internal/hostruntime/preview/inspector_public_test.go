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

// TestInspectorPublicCaptureAndReplay proves the default-audience ephemeral
// path is inspectable: public HTTP requests are captured sanitized under the
// lease ID (never the opaque carrier route), viewed without secrets, replayed
// byte-exact to the same origin, and purged when the lease ends.
func TestInspectorPublicCaptureAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var hits atomic.Int32
	var methods, uris, auths [4]string
	var bodies [4]string
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		index := int(hits.Add(1)) - 1
		payload, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if index < len(methods) {
			methods[index], uris[index], auths[index], bodies[index] = request.Method, request.URL.RequestURI(), request.Header.Get("Authorization"), string(payload)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"stored":true,"token":"zzz"}`)
	}))
	defer origin.Close()

	store := inspector.NewStore()
	registry := inspector.NewRegistry()
	grantNow := time.Now().UTC()
	grantDecision := func(principal, action string) inspectorapi.Decision {
		return inspectorapi.Decision{Principal: principal, Owner: "owner_01", ResourceKind: "preview", ResourceID: "preview-public", RouteID: "preview-public", ResourceGeneration: 2, RouteGeneration: 2, TargetGeneration: 2, IssuedAt: grantNow, ExpiresAt: grantNow.Add(time.Minute)}
	}
	grants := map[string]inspectorapi.Decision{
		"grant_owner_inspect": grantDecision("owner_01", "inspect"),
		"grant_owner_replay":  grantDecision("owner_01", "replay"),
	}
	grantActions := map[string]string{"grant_owner_inspect": "inspect", "grant_owner_replay": "replay"}
	service, err := inspectorapi.New(inspectorapi.Config{Store: store, Registry: registry, Authorize: func(_ context.Context, grantToken, kind, resource, route, action string) (inspectorapi.Decision, error) {
		decision, ok := grants[grantToken]
		if !ok || grantActions[grantToken] != action || kind != "preview" || resource != "preview-public" || route != "preview-public" {
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
	call := func(method, path string, body any, grant string) (int, []byte) {
		t.Helper()
		callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer callCancel()
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = strings.NewReader(string(raw))
		}
		request, err := http.NewRequestWithContext(callCtx, method, daemon.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if grant != "" {
			request.Header.Set("X-Paperboat-Inspector-Grant", grant)
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

	lease := Lease{ID: "preview-public", AccountID: identity.AccountID, Generation: 2, AccessMode: "public", Endpoint: "https://public.example.test", Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String()}, LeaseDeadline: time.Now().UTC().Add(time.Hour)}
	if status, payload := call(http.MethodPut, "/v1/inspector/policy", map[string]any{"resource_kind": "preview", "resource_id": lease.ID, "route_id": lease.ID, "enabled": true, "capture_request_body": true, "capture_response_body": true, "capture_raw": true}, "grant_owner_replay"); status != http.StatusOK {
		t.Fatalf("enable = %d: %s", status, payload)
	}

	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: hub, Identity: identity, RouteID: "pvc_rte_pub", Inspector: store, Registry: registry, DialOrigin: func(ctx context.Context, target LeaseTarget) (io.ReadWriteCloser, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
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
	open := testPreviewStreamOpen(identity, "pvc_rte_pub", "request-public")
	if err := connectorprotocol.WriteStreamOpen(stream, open); err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(stream, "POST /submit?session=secret HTTP/1.1\r\nHost: public.example.test\r\nAuthorization: Bearer client-secret\r\nContent-Type: application/json\r\nContent-Length: 31\r\nConnection: close\r\n\r\n{\"item\":\"book\",\"key\":\"hunter2\"}")
	response, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	_ = stream.Close()
	if err != nil || string(body) != `{"stored":true,"token":"zzz"}` {
		t.Fatalf("client bytes altered: %q err=%v", body, err)
	}
	if hits.Load() != 1 || methods[0] != "POST" || uris[0] != "/submit?session=secret" || auths[0] != "Bearer client-secret" || bodies[0] != `{"item":"book","key":"hunter2"}` {
		t.Fatalf("origin saw %q %q %q %q hits=%d", methods[0], uris[0], auths[0], bodies[0], hits.Load())
	}

	status, payload := call(http.MethodGet, "/v1/inspector/records?kind=preview&resource="+lease.ID+"&route="+lease.ID+"&limit=10", nil, "grant_owner_inspect")
	if status != http.StatusOK {
		t.Fatalf("list = %d: %s", status, payload)
	}
	var page struct {
		Records []struct {
			ID               string `json:"id"`
			URL              string `json:"url"`
			ResponseBody     string `json:"response_body"`
			ReplayIneligible string `json:"replay_ineligible"`
		} `json:"records"`
	}
	if err := json.Unmarshal(payload, &page); err != nil || len(page.Records) != 1 {
		t.Fatalf("page = %s err=%v", payload, err)
	}
	record := page.Records[0]
	if strings.Contains(record.URL, "secret") || strings.Contains(record.ResponseBody, "zzz") || strings.Contains(record.ResponseBody, "hunter2") {
		t.Fatalf("sanitized record leaked: %+v", record)
	}
	if record.ReplayIneligible != "" {
		t.Fatalf("complete public capture must be replayable: %+v", record)
	}

	status, payload = call(http.MethodPost, "/v1/inspector/replay", map[string]any{"resource_kind": "preview", "resource_id": lease.ID, "route_id": lease.ID, "capture_id": record.ID, "idempotency_key": "replay_public_01"}, "grant_owner_replay")
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, payload)
	}
	var replay struct {
		OperationID    string `json:"operation_id"`
		SideEffects    string `json:"side_effects"`
		ResponseStatus int    `json:"response_status"`
	}
	if err := json.Unmarshal(payload, &replay); err != nil || replay.OperationID == "" || replay.SideEffects == "" || replay.ResponseStatus != 200 {
		t.Fatalf("replay result = %s err=%v", payload, err)
	}
	if hits.Load() != 2 || methods[1] != "POST" || uris[1] != "/submit?session=secret" || bodies[1] != `{"item":"book","key":"hunter2"}` {
		t.Fatalf("replay did not preserve bytes: %q %q %q hits=%d", methods[1], uris[1], bodies[1], hits.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("carrier did not stop")
	}
	_ = carrier.Close(context.Background())
	if status, _ := call(http.MethodGet, "/v1/inspector/records?kind=preview&resource="+lease.ID+"&route="+lease.ID+"&limit=10", nil, "grant_owner_inspect"); status != http.StatusUnauthorized && status != http.StatusNotFound && status != http.StatusGone {
		t.Fatalf("purged list = %d, want denial", status)
	}
}

// TestInspectorTCPStaysOpaqueOnPublicPath proves non-HTTP bytes on the public
// path keep the exact raw behavior and create no HTTP captures, matching the
// contract that opaque TCP is outside request/body inspection and replay.
func TestInspectorTCPStaysOpaqueOnPublicPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()

	store := inspector.NewStore()
	registry := inspector.NewRegistry()
	identity := testPreviewCarrierIdentity(1)
	pair := newPreviewCarrierPair(t, ctx, identity)
	defer pair.close()
	hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	lease := Lease{ID: "preview-tcp", AccountID: identity.AccountID, Generation: 1, AccessMode: "public", Endpoint: "https://tcp.example.test", Target: LeaseTarget{Scheme: "tcp", Address: listener.Addr().String()}, LeaseDeadline: time.Now().UTC().Add(time.Hour)}
	if err := store.SetPolicy(lease.ID, inspector.ResourcePolicy{Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true, CaptureRaw: true}); err != nil {
		t.Fatal(err)
	}
	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Hub: hub, Identity: identity, RouteID: "pvc_rte_tcp", Inspector: store, Registry: registry})
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
	open := testPreviewStreamOpen(identity, "pvc_rte_tcp", "request-tcp")
	if err := connectorprotocol.WriteStreamOpen(stream, open); err != nil {
		t.Fatal(err)
	}
	payload := []byte("opaque-database-bytes\x00\xff")
	if _, err := stream.Write(payload); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, echo); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if string(echo) != string(payload) {
		t.Fatalf("tcp bytes altered: %q", echo)
	}
	now := time.Now().UTC()
	credential := inspector.Credential{PrincipalID: "user_01", Action: inspector.ActionInspect, ResourceID: lease.ID, AuthorityReadAt: now, ExpiresAt: now.Add(time.Minute)}
	page, err := store.List(context.Background(), credential, "", 10)
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("tcp must not create HTTP captures: %+v err=%v", page, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("carrier did not stop")
	}
}
