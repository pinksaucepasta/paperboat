package edgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
)

const runtimeHostTestTicket = browserTerminalTicketPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// runtimeHostEdge is the whole edge handler with one machine route (machine.runtime.example.test) and counters for
// the two things a refused request must never reach: the control plane's admission and the machine itself.
type runtimeHostEdge struct {
	url             string
	hostname        string
	admissions      atomic.Int32
	admittedHost    atomic.Value
	machineRequests atomic.Int32
}

func newRuntimeHostEdge(t *testing.T) *runtimeHostEdge {
	t.Helper()
	identity := testEdgePreviewIdentity(1, 1)
	server, client := testEdgePreviewCarrierPair(t, identity)
	t.Cleanup(func() { server.Close(); client.Close() })
	registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	value := DataCarrierPreviewRoute{RouteID: "runtime_browser", Hostname: "machine.runtime.example.test", Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
	if err := registry.Attach(value); err != nil {
		t.Fatal(err)
	}
	hub := NewBrowserTerminalHub()
	t.Cleanup(hub.Close)
	if err := hub.ActivateRoute(value.RouteID, BrowserTerminalRouteFence{Identity: identity, Revision: value.Revision, AttachmentGeneration: value.AttachmentGeneration}); err != nil {
		t.Fatal(err)
	}
	e := &runtimeHostEdge{hostname: value.Hostname}
	machine := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { e.machineRequests.Add(1) }))
	t.Cleanup(machine.Close)
	machineURL, err := url.Parse(machine.URL)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := New(Config{
		PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewPreviewCarrierRouteMatcher(registry),
		RuntimeCarrierTransport: browserTerminalTestRoundTripper{target: machineURL}, BrowserTerminalHub: hub, BrowserTerminalEdgeHost: "edge.example.test",
		BrowserTerminal: browserTerminalAdmissionFunc(func(_ context.Context, _, _, host string) (control.BrowserTerminalAdmission, error) {
			e.admissions.Add(1)
			e.admittedHost.Store(host)
			return control.BrowserTerminalAdmission{}, &control.BrowserTerminalRefused{Reason: terminalend.NoAccess}
		}),
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(policy)
	t.Cleanup(edge.Close)
	e.url = "ws" + strings.TrimPrefix(edge.URL, "http")
	return e
}

func (e *runtimeHostEdge) dial(t *testing.T, host, path string, subprotocols ...string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	header := http.Header{"Origin": {"https://dashboard.example.test"}}
	return websocket.Dial(ctx, e.url+path, &websocket.DialOptions{Host: host, HTTPHeader: header, Subprotocols: subprotocols, CompressionMode: websocket.CompressionDisabled})
}

// The browser terminal is recognised by its path on a machine's runtime hostname. A request without a valid ticket
// is refused there and never reaches the control plane or, through the edge's carrier, the machine's own route.
func TestBrowserTerminalOnRuntimeHostNeedsTicket(t *testing.T) {
	e := newRuntimeHostEdge(t)
	for _, tc := range []struct {
		name, path   string
		subprotocols []string
	}{
		{"no subprotocol", "/v1/browser-terminal", nil},
		{"browser subprotocol without a ticket", "/v1/browser-terminal", []string{browserTerminalSubprotocol}},
		{"ticket without the browser subprotocol", "/v1/browser-terminal", []string{runtimeHostTestTicket}},
		{"malformed ticket", "/v1/browser-terminal", []string{browserTerminalSubprotocol, browserTerminalTicketPrefix + "short"}},
		{"query string", "/v1/browser-terminal?x=1", []string{browserTerminalSubprotocol, runtimeHostTestTicket}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connection, response, err := e.dial(t, e.hostname, tc.path, tc.subprotocols...)
			if err == nil {
				connection.CloseNow()
				t.Fatal("a browser terminal request without a valid ticket was upgraded")
			}
			if response == nil || response.StatusCode == http.StatusSwitchingProtocols {
				t.Fatalf("response = %v, want an HTTP refusal", response)
			}
		})
	}
	// A plain GET (no upgrade) on the same path is refused too.
	request, _ := http.NewRequest(http.MethodGet, "http"+strings.TrimPrefix(e.url, "ws")+"/v1/browser-terminal", nil)
	request.Host = e.hostname
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatalf("a plain GET on the browser terminal path got %d", response.StatusCode)
	}
	if e.admissions.Load() != 0 || e.machineRequests.Load() != 0 {
		t.Fatalf("a refused request reached the control plane (%d) or the machine (%d)", e.admissions.Load(), e.machineRequests.Load())
	}
}

// With the ticket subprotocol the request on the runtime hostname goes to admission, named by the request's Host.
func TestBrowserTerminalServedOnRuntimeHostPath(t *testing.T) {
	e := newRuntimeHostEdge(t)
	connection, _, err := e.dial(t, e.hostname, "/v1/browser-terminal", browserTerminalSubprotocol, runtimeHostTestTicket)
	if err != nil {
		t.Fatalf("the upgrade must succeed so the admission answer can be read: %v", err)
	}
	defer connection.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err = connection.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Reason != "no_access" {
		t.Fatalf("browser saw %v, want the control plane's refusal no_access", err)
	}
	if host, _ := e.admittedHost.Load().(string); e.admissions.Load() != 1 || host != e.hostname {
		t.Fatalf("admission ran %d times for host %q, want once for %q", e.admissions.Load(), host, e.hostname)
	}
}

// The browser terminal is no longer served under the edge's own name: its old path is not found and never admitted.
func TestOldEdgeHostPathNoLongerServed(t *testing.T) {
	e := newRuntimeHostEdge(t)
	connection, response, err := e.dial(t, "edge.example.test", "/v1/browser-terminal/"+e.hostname, browserTerminalSubprotocol, runtimeHostTestTicket)
	if err == nil {
		connection.CloseNow()
		t.Fatal("the edge's own hostname still serves the browser terminal")
	}
	if response == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("response = %v, want 404", response)
	}
	if e.admissions.Load() != 0 || e.machineRequests.Load() != 0 {
		t.Fatalf("the old path reached the control plane (%d) or the machine (%d)", e.admissions.Load(), e.machineRequests.Load())
	}
}
