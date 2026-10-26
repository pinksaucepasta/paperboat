package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Streaming headers are not a streaming route: only the path allow-list lifts the request timeout.
func TestStreamingHeadersDoNotLiftTheRequestTimeoutOnOrdinaryRoutes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The handler is slow. It returns when the request context ends, or after a bound so that a
	// broken timeout fails the test instead of hanging it.
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return // timed out: the middleware answers
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	})
	tests := []struct {
		name   string
		header map[string]string
	}{
		{name: "event stream accept", header: map[string]string{"Accept": "text/event-stream"}},
		{name: "websocket upgrade", header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := requestID(timeout(20*time.Millisecond, logger, slow))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil)
			for name, value := range test.header {
				req.Header.Set(name, value)
			}
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"provider_unavailable"`) {
				t.Fatalf("status = %d, body = %s; want the request timeout", rec.Code, rec.Body.String())
			}
		})
	}
}

// A streaming route keeps its request context and its response past the server's ReadTimeout,
// which applies to every other request on the connection.
func TestStreamingRouteOutlivesServerReadTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.RequestTimeout = 20 * time.Millisecond
	router := NewRouter(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OverrideHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
				t.Error("streaming request context was canceled by the server read timeout")
			case <-time.After(150 * time.Millisecond):
			}
			_, _ = w.Write([]byte("done"))
		}),
	})
	server := httptest.NewUnstartedServer(router)
	server.Config.ReadTimeout = 50 * time.Millisecond
	server.Start()
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/tunnels/tun_1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "done" {
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
}

// An allow-listed event route streams only when asked for its event stream; its JSON representation is
// an ordinary request and keeps the request timeout.
func TestEventRouteJSONKeepsTheRequestTimeout(t *testing.T) {
	for _, c := range []struct {
		accept    string
		streaming bool
	}{{"", false}, {"application/json", false}, {"text/event-stream", true}} {
		req := httptest.NewRequest(http.MethodGet, "/v1/previews/prv_1/events", nil)
		if c.accept != "" {
			req.Header.Set("Accept", c.accept)
		}
		if got := isStreamingRoute(req); got != c.streaming {
			t.Fatalf("events with Accept %q: streaming=%v, want %v", c.accept, got, c.streaming)
		}
	}
}

// The edge's browser-terminal control socket is a websocket that outlives the request timeout: behind the timeout
// and gzip wrappers (the edge sends a bearer credential) its upgrade wrote 101 and then failed to hijack, so every browser attach died within a millisecond.
func TestEdgeBrowserTerminalControlUpgradesPastTheRequestTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.RequestTimeout = 20 * time.Millisecond
	router := NewRouter(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OverrideHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			kind, message, err := connection.Read(r.Context())
			if err == nil {
				time.Sleep(60 * time.Millisecond) // past the request timeout
				if r.Context().Err() != nil {
					return
				}
				_ = connection.Write(r.Context(), kind, message)
			}
		}),
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/edge/browser-terminal/control", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer edge"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if err := connection.Write(ctx, websocket.MessageText, []byte("ticket")); err != nil {
		t.Fatal(err)
	}
	if _, message, err := connection.Read(ctx); err != nil || string(message) != "ticket" {
		t.Fatalf("echo = %q, %v; want the socket to outlive the request timeout", message, err)
	}
}
