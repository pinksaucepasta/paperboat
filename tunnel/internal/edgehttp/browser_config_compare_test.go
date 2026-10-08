package edgehttp

import (
	"context"
	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type browserConfigCompareAdmissionFunc func(context.Context, string, string, string) (control.BrowserConfigCompareAdmission, error)

func (f browserConfigCompareAdmissionFunc) Admit(ctx context.Context, ticket, origin, host string) (control.BrowserConfigCompareAdmission, error) {
	return f(ctx, ticket, origin, host)
}
func TestRuntimeBrowserConfigCompareUsesSeparateOpaqueBridgeAndRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	defer server.Close()
	defer client.Close()
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	route := DataCarrierPreviewRoute{RouteID: "runtime_compare", Hostname: "machine.runtime.example.test", Kind: datacarrier.RuntimeCarrierRoute, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
	if err = registry.Attach(route); err != nil {
		t.Fatal(err)
	}
	revoked := make(chan struct{})
	hostDone := make(chan struct{})
	var closeHost sync.Once
	defer closeHost.Do(func() { close(hostDone) })
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != browserConfigComparePath || r.Header.Get("Authorization") != "Bearer compare-host-credential" || r.Header.Get("Origin") != "" || r.Header.Get("Sec-WebSocket-Protocol") != browserConfigCompareSubprotocol {
			t.Errorf("comparison upstream grant/path incorrect")
			http.Error(w, "forbidden", 403)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{browserConfigCompareSubprotocol}, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		kind, record, err := conn.Read(r.Context())
		if err != nil || kind != websocket.MessageBinary || string(record) != "opaque-browser-tls" {
			t.Error("opaque TLS request lost")
			return
		}
		if err = conn.Write(r.Context(), websocket.MessageBinary, []byte("opaque-host-tls")); err != nil {
			return
		}
		select {
		case <-hostDone:
		case <-r.Context().Done():
		}
	}))
	defer host.Close()
	hostURL, _ := url.Parse(host.URL)
	policy, err := New(Config{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test", MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewPreviewCarrierRouteMatcher(registry), RuntimeCarrierTransport: browserTerminalTestRoundTripper{target: hostURL}, BrowserTerminalEdgeHost: "edge.example.test", BrowserConfigCompare: browserConfigCompareAdmissionFunc(func(_ context.Context, ticket, origin, requestedHost string) (control.BrowserConfigCompareAdmission, error) {
		if ticket != strings.Repeat("A", 43) || origin != "https://dashboard.example.test" || requestedHost != route.Hostname {
			t.Error("comparison admission misbound")
		}
		return control.BrowserConfigCompareAdmission{Credential: "compare-host-credential", AssignmentID: "assignment_1", AttachmentID: "attachment_1", Closed: revoked, Close: func() {}}, nil
	})}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(policy)
	defer edge.Close()
	defer closeHost.Do(func() { close(hostDone) })
	header := http.Header{}
	header.Set("Origin", "https://dashboard.example.test")
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(edge.URL, "http")+browserConfigComparePath+"/"+route.Hostname, &websocket.DialOptions{Host: "edge.example.test", HTTPHeader: header, Subprotocols: []string{browserConfigCompareSubprotocol, browserTerminalTicketPrefix + strings.Repeat("A", 43)}})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		t.Fatalf("comparison dial status=%d err=%v", status, err)
	}
	defer conn.CloseNow()
	if err = conn.Write(ctx, websocket.MessageBinary, append([]byte{browserTerminalTLSDiscriminator}, []byte("opaque-browser-tls")...)); err != nil {
		t.Fatal(err)
	}
	kind, record, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(record) != "\x00opaque-host-tls" {
		t.Fatalf("opaque reply=%q err=%v", record, err)
	}
	close(revoked)
	if _, _, err = conn.Read(ctx); err == nil {
		t.Fatal("revoked comparison remained open")
	}
}
