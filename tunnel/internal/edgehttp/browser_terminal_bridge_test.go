package edgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBrowserTerminalBridgeFailureAndRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	identity := testEdgePreviewIdentity(1, 1)
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	if err := hub.ActivateRoute("route_bridge", fence); err != nil {
		t.Fatal(err)
	}
	key := BrowserTerminalSessionKey{RouteID: "route_bridge", TerminalSessionID: "term_bridge", ProcessGeneration: identity.ProcessGeneration}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			kind, message, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), kind, message); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	completed := make(chan error, 2)
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, err := hub.Subscribe(key, fence, r.URL.Query().Get("attachment"))
		if err != nil {
			completed <- err
			return
		}
		defer sub.Close()
		host, _, err := websocket.Dial(r.Context(), "ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
		if err != nil {
			completed <- err
			return
		}
		browser, err := websocket.Accept(w, r, nil)
		if err != nil {
			host.CloseNow()
			completed <- err
			return
		}
		completed <- bridgeBrowserTerminal(r.Context(), browser, host, sub)
	}))
	defer edge.Close()
	dial := func(id string) *websocket.Conn {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(edge.URL, "http")+"?attachment="+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	bad := dial("browser_bad")
	if err := bad.Write(ctx, websocket.MessageText, []byte("PRIVATE-terminal-data")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if !errors.Is(err, ErrBrowserTerminalMessage) {
			t.Fatalf("lost protocol cause: %T", err)
		}
	case <-ctx.Done():
		t.Fatal("bridge workers did not finish")
	}
	bad.CloseNow()
	good := dial("browser_good")
	if err := good.Write(ctx, websocket.MessageBinary, []byte{browserTerminalTLSDiscriminator, 1, 2}); err != nil {
		t.Fatal(err)
	}
	kind, message, err := good.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(message) != string([]byte{browserTerminalTLSDiscriminator, 1, 2}) {
		t.Fatal("bridge did not recover")
	}
	good.Close(websocket.StatusNormalClosure, "")
	select {
	case err := <-completed:
		if !browserTerminalExpected(err) {
			t.Fatalf("normal close became failure: %T", err)
		}
	case <-ctx.Done():
		t.Fatal("normal close workers did not finish")
	}
}

func TestBrowserTerminalExpectedRequiresEveryLeaf(t *testing.T) {
	if !browserTerminalExpected(errors.Join(context.Canceled, websocket.CloseError{Code: websocket.StatusNormalClosure})) {
		t.Fatal("normal departure")
	}
	for _, err := range []error{errors.Join(context.Canceled, syscall.EIO), errors.Join(websocket.CloseError{Code: websocket.StatusNormalClosure}, context.DeadlineExceeded)} {
		if browserTerminalExpected(err) {
			t.Fatal("operational cause hidden")
		}
	}
}
