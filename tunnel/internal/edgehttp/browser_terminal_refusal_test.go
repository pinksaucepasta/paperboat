package edgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
)

// Terminal slice 7 (Lane K plan): every refusal after the ticket parses is a WebSocket close the browser can read, with
// a 4xxx code and a typed reason, instead of an HTTP status JavaScript cannot see. This runs the whole edge handler.
func TestBrowserTerminalRefusalsAreClosesTheBrowserCanRead(t *testing.T) {
	closedAdmission := make(chan struct{})
	admitted := control.BrowserTerminalAdmission{Credential: "host-credential", TerminalSessionID: "term_1", AttachmentID: "attach_1", Closed: closedAdmission, Close: func() {}}
	for _, tc := range []struct {
		name         string
		admit        func() (control.BrowserTerminalAdmission, error)
		activateHub  bool
		hostDown     bool
		wantCode     int
		wantReason   string
		wantReported bool
	}{
		{"no access", func() (control.BrowserTerminalAdmission, error) {
			return control.BrowserTerminalAdmission{}, &control.BrowserTerminalRefused{Reason: terminalend.NoAccess}
		}, true, false, 4404, "no_access", false},
		{"terminal ended", func() (control.BrowserTerminalAdmission, error) {
			return control.BrowserTerminalAdmission{}, &control.BrowserTerminalRefused{Reason: terminalend.Ended}
		}, true, false, 4409, "ended", false},
		{"machine unavailable per the control plane", func() (control.BrowserTerminalAdmission, error) {
			return control.BrowserTerminalAdmission{}, &control.BrowserTerminalRefused{Reason: terminalend.MachineUnavailable}
		}, true, false, 4503, "machine_unavailable", true},
		{"control plane unreachable", func() (control.BrowserTerminalAdmission, error) {
			return control.BrowserTerminalAdmission{}, errors.New("dial control: connection refused")
		}, true, false, 4504, "edge_unavailable", true},
		{"incomplete admission", func() (control.BrowserTerminalAdmission, error) {
			return control.BrowserTerminalAdmission{Credential: "host-credential"}, nil
		}, true, false, 4504, "edge_unavailable", true},
		{"carrier replaced under the ticket", func() (control.BrowserTerminalAdmission, error) { return admitted, nil }, false, false, 4503, "machine_unavailable", true},
		{"host does not answer", func() (control.BrowserTerminalAdmission, error) { return admitted, nil }, true, true, 4503, "machine_unavailable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			value := DataCarrierPreviewRoute{RouteID: "runtime_browser", Hostname: "machine.runtime.example.test", Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
			if err := registry.Attach(value); err != nil {
				t.Fatal(err)
			}
			hub := NewBrowserTerminalHub()
			defer hub.Close()
			if tc.activateHub {
				if err := hub.ActivateRoute(value.RouteID, BrowserTerminalRouteFence{Identity: identity, Revision: value.Revision, AttachmentGeneration: value.AttachmentGeneration}); err != nil {
					t.Fatal(err)
				}
			}
			hostURL := &url.URL{Host: "127.0.0.1:1"} // nothing listens: the carrier transport cannot reach the host
			if !tc.hostDown {
				host := httptest.NewServer(http.NotFoundHandler())
				defer host.Close()
				if hostURL, err = url.Parse(host.URL); err != nil {
					t.Fatal(err)
				}
			}
			reported := make(chan error, 4)
			policy, err := New(Config{
				PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
				MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewPreviewCarrierRouteMatcher(registry),
				RuntimeCarrierTransport: browserTerminalTestRoundTripper{target: hostURL}, BrowserTerminalHub: hub, BrowserTerminalEdgeHost: "edge.example.test",
				OnFailure: func(_ context.Context, err error) { reported <- err },
				BrowserTerminal: browserTerminalAdmissionFunc(func(context.Context, string, string, string) (control.BrowserTerminalAdmission, error) {
					return tc.admit()
				}),
			}, http.NotFoundHandler())
			if err != nil {
				t.Fatal(err)
			}
			edge := httptest.NewServer(policy)
			defer edge.Close()
			header := make(http.Header)
			header.Set("Origin", "https://dashboard.example.test")
			connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(edge.URL, "http")+"/v1/browser-terminal/"+value.Hostname, &websocket.DialOptions{
				Host: "edge.example.test", HTTPHeader: header, CompressionMode: websocket.CompressionDisabled,
				Subprotocols: []string{browserTerminalSubprotocol, browserTerminalTicketPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			})
			if err != nil {
				t.Fatalf("the upgrade must succeed so the refusal can be read: %v", err)
			}
			defer connection.CloseNow()
			_, _, err = connection.Read(ctx)
			var closed websocket.CloseError
			if !errors.As(err, &closed) || int(closed.Code) != tc.wantCode || closed.Reason != tc.wantReason {
				t.Fatalf("browser saw %v, want close %d %q", err, tc.wantCode, tc.wantReason)
			}
			select {
			case err := <-reported:
				if !tc.wantReported {
					t.Fatalf("a user-level refusal was filed as an edge failure: %v", err)
				}
			case <-time.After(300 * time.Millisecond):
				if tc.wantReported {
					t.Fatal("an edge-side refusal was not reported (the old silent 503)")
				}
			}
		})
	}
}
