package tunnelmanager

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

// TestInspectorDurableRegistersReplayBinding proves the durable forwarding
// choke point refreshes the current replay binding from the admitted decision
// on the same shared registry the ephemeral preview path uses. Deliberate
// replay then reuses exact bytes to the same origin through the route's
// transport and TLS policy.
func TestInspectorDurableRegistersReplayBinding(t *testing.T) {
	var hits int
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hits++
		if request.Host != "public.example.test" {
			t.Errorf("origin Host changed: %q", request.Host)
		}
		if request.Method != "POST" || request.URL.RequestURI() != "/submit?session=secret" || request.Header.Get("Authorization") != "Bearer client-secret" {
			t.Errorf("origin saw %s %s auth=%q", request.Method, request.URL.RequestURI(), request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"stored":true}`)
	}))
	defer origin.Close()

	store := inspector.NewStore()
	registry := inspector.NewRegistry()
	if err := store.SetPolicy("route_durable", inspector.ResourcePolicy{
		Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true, CaptureRaw: true,
	}); err != nil {
		t.Fatal(err)
	}
	forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, Inspector: store, Registry: registry}
	route := inspectorTestRoute(origin.Listener.Addr().String())
	route.ID = "route_durable"
	route.PreserveHost = true

	binding := connectorprotocol.IngressBinding{Hostname: "public.example.test", PathPrefix: "/", ResourceGeneration: 7, RouteGeneration: 7, TargetGeneration: 7}
	ctx := context.WithValue(context.Background(), ingressBindingKey{}, binding)
	ctx = context.WithValue(ctx, ingressExpiryKey{}, time.Now().UTC().Add(time.Second))

	daemon, client := net.Pipe()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- forwarder.serveHTTP(ctx, daemon, route, forwarder.Transport)
	}()
	clientRequest, err := http.NewRequest(http.MethodPost, "http://public.example.test/submit?session=secret", strings.NewReader(`{"item":"book"}`))
	if err != nil {
		t.Fatal(err)
	}
	clientRequest.Header.Set("Content-Type", "application/json")
	clientRequest.Header.Set("Authorization", "Bearer client-secret")
	writeErr := make(chan error, 1)
	go func() { writeErr <- clientRequest.Write(client) }()
	response, err := http.ReadResponse(bufio.NewReader(client), clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	_ = client.Close()
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
	if string(responseBody) != `{"stored":true}` || hits != 1 {
		t.Fatalf("forwarding altered: %q hits=%d", responseBody, hits)
	}

	current, ok := registry.Current("route_durable", time.Now().UTC())
	if !ok || current.ResourceGeneration != 7 || current.Forward == nil {
		t.Fatalf("durable replay binding missing: %+v ok=%v", current, ok)
	}
	if _, ok := registry.Current("route_durable", time.Now().UTC().Add(30*time.Second)); !ok {
		t.Fatal("live raw capture lost its target snapshot when the original ingress decision expired")
	}
	now := time.Now().UTC()
	manager := inspector.NewManager(store)
	result, err := manager.Replay(context.Background(), inspector.ReplayRequest{
		PrincipalID: "user_01", ResourceID: "route_durable", CaptureID: currentBindingCaptureID(t, store, now),
		ResourceGeneration: 7, RouteGeneration: 7, TargetGeneration: 7,
		IdempotencyKey: "replay_durable_01", AuthorityReadAt: now, ExpiresAt: now.Add(time.Minute),
	}, current.Forward)
	if err != nil || result.ResponseStatus != 200 || hits != 2 {
		t.Fatalf("durable replay = %+v err=%v hits=%d", result, err, hits)
	}
	forwarder.Transport.CloseIdleConnections()
	if _, ok := registry.Current("route_durable", time.Now().UTC()); ok {
		t.Fatal("closed local target remained replayable")
	}
	if result.Method != "POST" || !strings.Contains(result.URL, "/submit") || strings.Contains(result.URL, "secret") {
		t.Fatalf("replay result not sanitized: %+v", result)
	}
}

func currentBindingCaptureID(t *testing.T, store *inspector.Store, now time.Time) string {
	t.Helper()
	credential := inspector.Credential{
		PrincipalID: "user_01", Action: inspector.ActionInspect, ResourceID: "route_durable",
		ResourceGeneration: 7, RouteGeneration: 7, TargetGeneration: 7,
		AuthorityReadAt: now, ExpiresAt: now.Add(time.Minute),
	}
	page, err := store.List(context.Background(), credential, "", 10)
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("captures = %+v err=%v", page, err)
	}
	return page.Records[0].ID
}
