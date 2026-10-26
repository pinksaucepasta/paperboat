package control

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBrowserTerminalControlChannelIsNodeBoundAndFailClosed(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/edge/browser-terminal/control" || r.Header.Get("Authorization") != "Bearer "+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || r.Header.Get("X-Paperboat-Edge-Node-ID") != "edge_1" || r.Header.Get("X-Paperboat-Edge-Process-Epoch") != "epoch_01" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		kind, raw, err := connection.Read(r.Context())
		if err != nil || kind != websocket.MessageText || string(raw) != `{"ticket":"one-use","origin":"https://dashboard.example.test","host":"machine.runtime.example.test"}` {
			return
		}
		requests <- struct{}{}
		if connection.Write(r.Context(), websocket.MessageText, []byte(`{"credential":"host-credential","terminal_session_id":"term_1","attachment_id":"attach_1"}`)) != nil {
			return
		}
		// A revoked control channel must end the edge attachment even though
		// the host credential was already issued.
		_ = connection.Close(websocket.StatusPolicyViolation, "revoked")
	}))
	defer server.Close()
	httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: 3 * time.Second, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client := &BrowserTerminalClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "epoch_01"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admission, err := client.Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
	if err != nil || admission.Credential != "host-credential" || admission.TerminalSessionID != "term_1" || admission.AttachmentID != "attach_1" {
		t.Fatalf("admission: %v", err)
	}
	defer admission.Close()
	select {
	case <-requests:
	case <-ctx.Done():
		t.Fatal("control request did not reach server")
	}
	select {
	case <-admission.Closed:
	case <-ctx.Done():
		t.Fatal("revoked control did not close attachment")
	}
}

// The server refuses an admission by closing the control channel with a typed reason; the edge must learn it as a
// typed refusal (not a transport failure) and pass the reason on. A reason it does not know (an older server) is
// edge_unavailable, a retry.
func TestBrowserTerminalAdmissionRefusalsAreTyped(t *testing.T) {
	for _, tc := range []struct{ closeReason, want string }{
		{"bandwidth_quota_exhausted", "bandwidth_quota_exhausted"},
		{"no_access", "no_access"},
		{"machine_unavailable", "machine_unavailable"},
		{"expired", "expired"},
		{"ended: the terminal closed", "ended"},
		{"admission denied", "edge_unavailable"},
		{"", "edge_unavailable"},
	} {
		t.Run(tc.closeReason, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer connection.CloseNow()
				if _, _, err := connection.Read(r.Context()); err != nil {
					return
				}
				_ = connection.Close(websocket.StatusPolicyViolation, tc.closeReason)
			}))
			defer server.Close()
			httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: 3 * time.Second, Client: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			client := &BrowserTerminalClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "epoch_01"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = client.Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
			var refused *BrowserTerminalRefused
			if !errors.As(err, &refused) || string(refused.Reason) != tc.want {
				t.Fatalf("admission refused with %v, want reason %q", err, tc.want)
			}
		})
	}
}

// A live admission learns why the control plane ended it, so the edge can tell the browser.
func TestBrowserTerminalLiveAdmissionReportsWhyItEnded(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		if _, _, err := connection.Read(r.Context()); err != nil {
			return
		}
		_ = connection.Write(r.Context(), websocket.MessageText, []byte(`{"credential":"host-credential","terminal_session_id":"term_1","attachment_id":"attach_1"}`))
		_ = connection.Write(r.Context(), websocket.MessageText, []byte("ok"))
		_ = connection.Close(websocket.StatusPolicyViolation, "revoked")
	}))
	defer server.Close()
	httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: 3 * time.Second, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client := &BrowserTerminalClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "epoch_01"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admission, err := client.Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	select {
	case <-admission.Closed:
	case <-ctx.Done():
		t.Fatal("admission did not end")
	}
	if reason := admission.EndReason(); reason != "revoked" {
		t.Fatalf("end reason = %q, want revoked", reason)
	}
}

// Production reaches the control plane through a reverse proxy. The server reads the edge's admission first and
// gives up after 5 s, so the edge must write its request as soon as the upgrade completes, and the bytes must survive
// a proxy that only forwards them.
func TestBrowserTerminalAdmissionCrossesAByteForwardingProxy(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		firstCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		kind, _, err := connection.Read(firstCtx)
		cancel()
		if err != nil || kind != websocket.MessageText {
			_ = connection.Close(websocket.StatusPolicyViolation, "invalid admission")
			return
		}
		if connection.Write(r.Context(), websocket.MessageText, []byte(`{"credential":"host-credential","terminal_session_id":"term_1","attachment_id":"attach_1"}`)) != nil {
			return
		}
		_, _, _ = connection.Read(r.Context())
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				downstream.Close()
				continue
			}
			go func() { _, _ = io.Copy(upstream, downstream); upstream.Close() }()
			go func() { _, _ = io.Copy(downstream, upstream); downstream.Close() }()
		}
	}()
	httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: "https://" + listener.Addr().String(), Credential: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: 3 * time.Second, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client := &BrowserTerminalClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "epoch_01"}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	admission, err := client.Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
	if err != nil || admission.Credential != "host-credential" {
		t.Fatalf("admission through proxy: %v", err)
	}
	admission.Close()
}

// Each step of the admission fails with its own category, so a failure says where it stopped.
func TestBrowserTerminalAdmissionFailureNamesItsStep(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"dial": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		},
		"first_read": func(w http.ResponseWriter, r *http.Request) {
			connection, err := websocket.Accept(w, r, nil)
			if err == nil {
				_, _, _ = connection.Read(r.Context())
				connection.CloseNow()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			httpClient, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: 3 * time.Second, Client: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			client := &BrowserTerminalClient{HTTP: httpClient, NodeID: "edge_1", ProcessEpoch: "epoch_01"}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			_, err = client.Admit(ctx, "one-use", "https://dashboard.example.test", "machine.runtime.example.test")
			var failure *RequestFailure
			if !errors.As(err, &failure) || failure.Category != name {
				t.Fatalf("got %#v, want category %s", err, name)
			}
		})
	}
}
