package edgehttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
)

// pb update: a browser terminal attached through the old worker generation keeps its output through
// the handover (no close, no gap) while a terminal opened mid-update lands on the new generation. When
// the old generation stops (its drain ended or reached runtimewire.DrainLimit) the browser is
// told to reconnect at once (moved); a revocation instead ends it with its own reason.
func TestHeldBrowserTerminalCrossesAWorkerHandover(t *testing.T) {
	for _, end := range []string{"drain limit", "revocation"} {
		t.Run(end, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			oldIdentity, newIdentity := testEdgePreviewIdentity(1, 1), testEdgePreviewIdentity(1, 2)
			newIdentity.SessionID = "session_02"
			oldServer, oldClient := testEdgePreviewCarrierPair(t, oldIdentity)
			defer oldServer.Close()
			defer oldClient.Close()
			newServer, newClient := testEdgePreviewCarrierPair(t, newIdentity)
			defer newServer.Close()
			defer newClient.Close()
			registry, err := NewDataCarrierPreviewRegistry(DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: "edge_epoch_1"})
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			oldRoute := DataCarrierPreviewRoute{RouteID: "runtime_browser", Hostname: "machine.runtime.example.test", Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: oldIdentity, Server: oldServer, ExpiresAt: time.Now().Add(time.Minute)}
			newRoute := oldRoute
			newRoute.Identity, newRoute.Server, newRoute.AttachmentGeneration = newIdentity, newServer, 2
			oldFence := BrowserTerminalRouteFence{Identity: oldIdentity, Revision: 1, AttachmentGeneration: 1}
			newFence := BrowserTerminalRouteFence{Identity: newIdentity, Revision: 1, AttachmentGeneration: 2}
			if err := registry.Attach(oldRoute); err != nil {
				t.Fatal(err)
			}
			hub := NewBrowserTerminalHub()
			defer hub.Close()
			if err := hub.ActivateRoute(oldRoute.RouteID, oldFence); err != nil {
				t.Fatal(err)
			}
			hostDone := make(chan struct{})
			defer close(hostDone)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{browserTerminalSubprotocol}})
				if err != nil {
					return
				}
				defer connection.CloseNow()
				select {
				case <-hostDone:
				case <-r.Context().Done():
				}
			}))
			defer host.Close()
			hostURL, err := url.Parse(host.URL)
			if err != nil {
				t.Fatal(err)
			}
			revoked := make(chan struct{})
			admissions := 0
			policy, err := New(Config{
				PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
				MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewPreviewCarrierRouteMatcher(registry),
				RuntimeCarrierTransport: browserTerminalTestRoundTripper{target: hostURL}, BrowserTerminalHub: hub, BrowserTerminalEdgeHost: "edge.example.test",
				BrowserTerminal: browserTerminalAdmissionFunc(func(context.Context, string, string, string) (control.BrowserTerminalAdmission, error) {
					admissions++
					closed := make(chan struct{})
					if admissions == 1 {
						closed = revoked
					}
					return control.BrowserTerminalAdmission{Credential: "host-credential", TerminalSessionID: "term_1", AttachmentID: "attach_" + strconv.Itoa(admissions), Closed: closed, Close: func() {},
						EndReason: func() terminalend.Reason {
							select {
							case <-closed:
								return terminalend.Revoked
							default:
								return ""
							}
						}}, nil
				}),
			}, http.NotFoundHandler())
			if err != nil {
				t.Fatal(err)
			}
			edge := httptest.NewServer(policy)
			defer edge.Close()
			dial := func() *websocket.Conn {
				header := make(http.Header)
				header.Set("Origin", "https://dashboard.example.test")
				connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(edge.URL, "http")+"/v1/browser-terminal/"+oldRoute.Hostname, &websocket.DialOptions{
					Host: "edge.example.test", HTTPHeader: header, CompressionMode: websocket.CompressionDisabled,
					Subprotocols: []string{browserTerminalSubprotocol, browserTerminalTicketPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
				})
				if err != nil {
					t.Fatal(err)
				}
				return connection
			}
			read := func(connection *websocket.Conn, want string) {
				t.Helper()
				if _, payload, err := connection.Read(ctx); err != nil || string(payload) != "\x01"+want {
					t.Fatalf("browser read %q, %v; want record %q", payload, err, want)
				}
			}
			held := dial()
			defer held.CloseNow()
			oldPublisher := registerWhenSubscribed(t, ctx, hub, BrowserTerminalSessionKey{RouteID: oldRoute.RouteID, TerminalSessionID: "term_1", ProcessGeneration: 1}, oldIdentity)
			if err := oldPublisher.Publish([]byte("r1")); err != nil {
				t.Fatal(err)
			}
			read(held, "r1")

			// The new worker's carrier takes the route; the old one is the standby with its held stream.
			if err := registry.Attach(newRoute); err != nil {
				t.Fatal(err)
			}
			if err := hub.ActivateRoute(newRoute.RouteID, newFence); err != nil {
				t.Fatal(err)
			}
			for _, record := range []string{"r2", "r3"} {
				if err := oldPublisher.Publish([]byte(record)); err != nil {
					t.Fatalf("the held terminal's output stopped at the handover: %v", err)
				}
				read(held, record)
			}
			fresh := dial()
			defer fresh.CloseNow()
			newPublisher := registerWhenSubscribed(t, ctx, hub, BrowserTerminalSessionKey{RouteID: newRoute.RouteID, TerminalSessionID: "term_1", ProcessGeneration: 2}, newIdentity)
			if err := newPublisher.Publish([]byte("n1")); err != nil {
				t.Fatal(err)
			}
			read(fresh, "n1")
			if err := oldPublisher.Publish([]byte("r4")); err != nil {
				t.Fatal(err)
			}
			read(held, "r4") // in order: the held browser never saw the new generation's record
			// A second update: a third generation takes the route; the first one still serves the held browser.
			thirdIdentity := testEdgePreviewIdentity(1, 3)
			thirdIdentity.SessionID = "session_03"
			if err := hub.ActivateRoute(oldRoute.RouteID, BrowserTerminalRouteFence{Identity: thirdIdentity, Revision: 1, AttachmentGeneration: 3}); err != nil {
				t.Fatal(err)
			}
			if err := oldPublisher.Publish([]byte("r5")); err != nil {
				t.Fatalf("the held terminal's output stopped at the second handover: %v", err)
			}
			read(held, "r5")

			want := websocket.StatusCode(terminalend.CodeMoved)
			if end == "revocation" {
				close(revoked)
				want = websocket.StatusCode(terminalend.CodeRevoked)
			} else {
				hub.DeactivateRoute(oldRoute.RouteID, oldFence) // the old carrier detached
			}
			_, _, err = held.Read(ctx)
			var closed websocket.CloseError
			if !errors.As(err, &closed) || closed.Code != want {
				t.Fatalf("held browser saw %v, want close %d", err, want)
			}
			if err := newPublisher.Publish([]byte("n2")); err != nil {
				t.Fatal(err)
			}
			read(fresh, "n2")
		})
	}
}

// registerWhenSubscribed registers the host's output publisher once the browser's edge bridge joined
// the session (it does so after admission, asynchronously to the browser's dial).
func registerWhenSubscribed(t *testing.T, ctx context.Context, hub *BrowserTerminalHub, key BrowserTerminalSessionKey, identity datacarrier.Identity) *BrowserTerminalPublisher {
	t.Helper()
	for {
		hub.mu.Lock()
		session := hub.sessions[key]
		joined := session != nil && len(session.subscribers) != 0
		hub.mu.Unlock()
		if joined {
			publisher, err := hub.RegisterPublisher(key, identity)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(publisher.Close)
			return publisher
		}
		select {
		case <-ctx.Done():
			t.Fatal("the browser never joined the terminal's output")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// An edge install keeps every machine generation, so a released edge process marks its hub with HandOver: whichever
// ends a browser terminal next, the host's carrier detaching after the drain's goaway (live, 2026-11-09: it came
// before any timer and ended the terminal as a plain drop, 1006) or the grace running out, the browser gets exactly one
// typed "moved" close (4301, before the process exits), and its reattach continues with the next record, nothing
// repeated and nothing lost.
func TestHandOverEndsBrowserTerminalsMovedAndReattachContinues(t *testing.T) {
	for _, end := range []string{"carrier detaches first", "grace ends first", "revoked while handing over"} {
		t.Run(end, func(t *testing.T) { handOverEndsBrowserTerminalsMoved(t, end) })
	}
}

// A browser terminal the control plane ends (a revocation) while the edge hands over keeps that typed reason; only a
// session the hub ended is a move.
func TestMovedByHandOverOnlyForASessionTheHubEnded(t *testing.T) {
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	if hub.MovedByHandOver(ErrBrowserTerminalHubEnded) {
		t.Fatal("moved before any hand over")
	}
	hub.HandOver(time.Hour)
	if !hub.MovedByHandOver(bridgeEnd{"shared_output", ErrBrowserTerminalHubEnded}) || !hub.MovedByHandOver(errors.Join(errors.New("x"), ErrBrowserTerminalHubEnded)) {
		t.Fatal("a session the hub ended after the hand over is not a move")
	}
	for _, cause := range []error{nil, context.Canceled, bridgeEnd{"browser_read", errors.New("eof")}, ErrIngressQuotaExhausted} {
		if hub.MovedByHandOver(cause) {
			t.Fatalf("%v counted as moved by the hand over", cause)
		}
	}
}

// Close cancels the grace timer: a closed hub has nothing left to move and no timer outlives it.
func TestHubCloseStopsTheHandOverTimer(t *testing.T) {
	hub := NewBrowserTerminalHub()
	hub.HandOver(time.Hour)
	timer := hub.handoverTimer
	if timer == nil {
		t.Fatal("HandOver kept no timer")
	}
	hub.HandOver(time.Hour) // a second call replaces it
	if timer.Stop() {
		t.Fatal("the first timer was still pending after a second HandOver")
	}
	pending := hub.handoverTimer
	_ = hub.Close()
	if pending.Stop() {
		t.Fatal("Close left the timer pending")
	}
}

func handOverEndsBrowserTerminalsMoved(t *testing.T, mode string) {
	carrierFirst := mode == "carrier detaches first"
	revoked := make(chan struct{})
	var logs lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
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
	route := DataCarrierPreviewRoute{RouteID: "runtime_browser", Hostname: "machine.runtime.example.test", Kind: runtimewire.RouteKind, EdgeProcessEpoch: "edge_epoch_1", Revision: 1, AttachmentGeneration: 1, Identity: identity, Server: server, ExpiresAt: time.Now().Add(time.Minute)}
	if err := registry.Attach(route); err != nil {
		t.Fatal(err)
	}
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	if err := hub.ActivateRoute(route.RouteID, BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	hostDone := make(chan struct{})
	defer close(hostDone)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{browserTerminalSubprotocol}})
		if err != nil {
			return
		}
		defer connection.CloseNow()
		select {
		case <-hostDone:
		case <-r.Context().Done():
		}
	}))
	defer host.Close()
	hostURL, err := url.Parse(host.URL)
	if err != nil {
		t.Fatal(err)
	}
	admissions := 0
	policy, err := New(Config{
		PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "tunnels.example.test", RuntimeBaseDomain: "runtime.example.test",
		MaxHeaderBytes: 4096, MaxBodyBytes: 4096, Routes: NewPreviewCarrierRouteMatcher(registry),
		RuntimeCarrierTransport: browserTerminalTestRoundTripper{target: hostURL}, BrowserTerminalHub: hub, BrowserTerminalEdgeHost: "edge.example.test",
		BrowserTerminal: browserTerminalAdmissionFunc(func(context.Context, string, string, string) (control.BrowserTerminalAdmission, error) {
			admissions++
			return control.BrowserTerminalAdmission{Credential: "host-credential", TerminalSessionID: "term_1", AttachmentID: "attach_" + strconv.Itoa(admissions), Closed: revoked, Close: func() {},
				EndReason: func() terminalend.Reason {
					select {
					case <-revoked:
						return terminalend.Revoked
					default:
						return ""
					}
				}}, nil
		}),
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(policy)
	defer edge.Close()
	dial := func() *websocket.Conn {
		header := make(http.Header)
		header.Set("Origin", "https://dashboard.example.test")
		connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(edge.URL, "http")+"/v1/browser-terminal/"+route.Hostname, &websocket.DialOptions{
			Host: "edge.example.test", HTTPHeader: header, CompressionMode: websocket.CompressionDisabled,
			Subprotocols: []string{browserTerminalSubprotocol, browserTerminalTicketPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return connection
	}
	read := func(connection *websocket.Conn, want string) {
		t.Helper()
		if _, payload, err := connection.Read(ctx); err != nil || string(payload) != "\x01"+want {
			t.Fatalf("browser read %q, %v; want record %q", payload, err, want)
		}
	}
	key := BrowserTerminalSessionKey{RouteID: route.RouteID, TerminalSessionID: "term_1", ProcessGeneration: 1}
	first := dial()
	defer first.CloseNow()
	publisher := registerWhenSubscribed(t, ctx, hub, key, identity)
	if err := publisher.Publish([]byte("r1")); err != nil {
		t.Fatal(err)
	}
	read(first, "r1")

	oldFence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if mode == "revoked while handing over" {
		hub.HandOver(time.Hour)
		close(revoked)
		_, _, err = first.Read(ctx)
		var closed websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.StatusCode(terminalend.CodeRevoked) {
			t.Fatalf("browser saw %v, want the revocation's own close %d", err, terminalend.CodeRevoked)
		}
		return
	}
	if carrierFirst {
		hub.HandOver(time.Hour)
		hub.DeactivateRoute(route.RouteID, oldFence) // the host's carrier left the old process
	} else {
		hub.HandOver(50 * time.Millisecond)
	}
	// Exactly one close, and it is the first thing after the last record: no other frame, no plain drop.
	_, _, err = first.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.StatusCode(terminalend.CodeMoved) {
		t.Fatalf("browser saw %v, want close %d (moved)", err, terminalend.CodeMoved)
	}

	// The edge's end log names the code it sent, so support can tell a move from a drop without the browser.
	for deadline := time.Now().Add(2 * time.Second); !strings.Contains(logs.String(), "close_sent=4301"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the end log lacks close_sent=4301: %q", logs.String())
		}
	}

	// The browser reattaches (to the replacement process in production) and continues with the next record.
	if carrierFirst {
		if err := hub.ActivateRoute(route.RouteID, oldFence); err != nil {
			t.Fatal(err)
		}
	}
	second := dial()
	defer second.CloseNow()
	next := registerWhenSubscribed(t, ctx, hub, key, identity)
	if err := next.Publish([]byte("r2")); err != nil {
		t.Fatal(err)
	}
	read(second, "r2")
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
