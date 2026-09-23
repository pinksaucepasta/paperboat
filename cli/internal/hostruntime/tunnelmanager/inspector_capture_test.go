package tunnelmanager

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

func inspectorTestRoute(address string) hoststate.TunnelConfigRoute {
	return hoststate.TunnelConfigRoute{
		ID: "route_01", Name: "default", Protocol: "http",
		MatchType: "exact", MatchHostname: "public.example.test",
		OriginScheme: "http", OriginAddress: address, PreserveHost: true,
		TLSVerification: "not_applicable", ConnectTimeoutMs: 1000,
		IdleTimeoutMs: 5000, MaxConcurrentStreams: 4, DesiredState: "active",
	}
}

func inspectorTestCredential(resourceID string) inspector.Credential {
	now := time.Now().UTC()
	return inspector.Credential{
		PrincipalID:     "user_01",
		Action:          inspector.ActionInspect,
		ResourceID:      resourceID,
		AuthorityReadAt: now,
		ExpiresAt:       now.Add(time.Minute),
	}
}

// tcpHalfCloseStream adapts a net.Pipe end to the half-close contract that
// serveTCP requires from real TCP connections. TCP semantics themselves are
// exercised against a real loopback listener; the pipe only stands in for the
// edge carrier stream.
type tcpHalfCloseStream struct {
	net.Conn
}

func (s tcpHalfCloseStream) CloseWrite() error { return nil }

// TestInspectorCapturePreservesStreaming proves the shared HTTP forwarding path
// captures a sanitized record without altering streamed bytes: a bounded
// chunked JSON origin response reaches the client exactly while the store
// holds redacted headers, redacted query values and redacted body secrets.
func TestInspectorCapturePreservesStreaming(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer client-secret" {
			t.Errorf("origin authorization = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Set-Cookie", "session=origin-secret")
		flusher, _ := writer.(http.Flusher)
		for _, chunk := range []string{`{"message":"hel`, `lo","password":"hun`, `ter2","n":`, `42}`} {
			_, _ = io.WriteString(writer, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer origin.Close()
	address := origin.Listener.Addr().String()

	store := inspector.NewStore()
	if err := store.SetPolicy("route_01", inspector.ResourcePolicy{
		Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true, CaptureRaw: true,
	}); err != nil {
		t.Fatal(err)
	}
	forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, Inspector: store}
	route := inspectorTestRoute(address)

	daemon, client := net.Pipe()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- forwarder.serveHTTP(context.Background(), daemon, route, forwarder.Transport)
	}()
	clientRequest, err := http.NewRequest(http.MethodPost, "http://public.example.test/api/items?session=secret", strings.NewReader(`{"message":"hello","password":"hunter2"}`))
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
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	_ = client.Close()
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
	const want = `{"message":"hello","password":"hunter2","n":42}`
	if string(body) != want {
		t.Fatalf("streamed bytes altered: got %q want %q", body, want)
	}

	page, err := store.List(context.Background(), inspectorTestCredential("route_01"), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("captures = %d, want 1", len(page.Records))
	}
	record := page.Records[0]
	if record.Method != "POST" || record.ResponseStatus != 200 || record.State != inspector.StateComplete {
		t.Fatalf("record = %+v", record)
	}
	if got := record.RequestHeaders.Get("Authorization"); got != "[redacted]" {
		t.Fatalf("request authorization leaked: %q", got)
	}
	if got := record.ResponseHeaders.Get("Set-Cookie"); got != "[redacted]" {
		t.Fatalf("response cookie leaked: %q", got)
	}
	if strings.Contains(record.URL, "session=secret") {
		t.Fatalf("query secret leaked: %q", record.URL)
	}
	if strings.Contains(string(record.RequestBody), "hunter2") || strings.Contains(string(record.ResponseBody), "hunter2") {
		t.Fatalf("body secret leaked: req=%s resp=%s", record.RequestBody, record.ResponseBody)
	}
	if !strings.Contains(string(record.ResponseBody), "hello") {
		t.Fatalf("non-sensitive body lost: %s", record.ResponseBody)
	}
	// Viewers must not open the inspection API even though forwarding worked.
	viewer := inspectorTestCredential("route_01")
	viewer.Action = inspector.ActionView
	if _, err := store.List(context.Background(), viewer, "", 10); err != inspector.ErrForbidden {
		t.Fatalf("viewer list = %v, want forbidden", err)
	}
}

// TestInspectorTCPStaysOpaque proves raw TCP forwarding creates no HTTP
// capture records: the byte stream is preserved exactly and the store stays
// empty for that resource.
func TestInspectorTCPStaysOpaque(t *testing.T) {
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
	if err := store.SetPolicy("route_tcp", inspector.ResourcePolicy{Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true}); err != nil {
		t.Fatal(err)
	}
	forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}, Inspector: store}
	// serveTCP requires a non-nil IngressAuthority as proof that upstream
	// admission ran; it never captures bodies regardless of the inspector.
	forwarder.IngressAuthority = func(ctx context.Context, open connectorprotocol.StreamOpen, claimed connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return claimed, nil
	}
	route := hoststate.TunnelConfigRoute{
		ID: "route_tcp", Protocol: "tcp", OriginScheme: "tcp", OriginAddress: listener.Addr().String(),
		ConnectTimeoutMs: 1000, IdleTimeoutMs: 5000, DesiredState: "active",
	}
	daemon, client := net.Pipe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- forwarder.serveTCP(context.Background(), tcpHalfCloseStream{daemon}, route) }()
	payload := []byte("opaque-database-bytes\x00\xff")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(client, echo); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	_ = daemon.Close()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
		t.Fatal("tcp serve did not finish")
	}
	if string(echo) != string(payload) {
		t.Fatalf("tcp bytes altered: %q", echo)
	}
	page, err := store.List(context.Background(), inspectorTestCredential("route_tcp"), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 {
		t.Fatalf("tcp must not create HTTP captures, got %d", len(page.Records))
	}
}

// TestInspectorDisabledForwarderUnchanged proves a nil store keeps the exact
// previous forwarding behavior for callers that never opt in.
func TestInspectorDisabledForwarderUnchanged(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, "origin-ok")
	}))
	defer origin.Close()
	forwarder := OriginStreamForwarder{Transport: &OriginHTTPTransport{}}
	route := inspectorTestRoute(origin.Listener.Addr().String())
	daemon, client := net.Pipe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- forwarder.serveHTTP(context.Background(), daemon, route, forwarder.Transport) }()
	request, _ := http.NewRequest(http.MethodGet, "http://public.example.test/", nil)
	go func() { _ = request.Write(client) }()
	response, err := http.ReadResponse(bufio.NewReader(client), request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	_ = client.Close()
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
	if string(body) != "origin-ok" {
		t.Fatalf("body = %q", body)
	}
}
