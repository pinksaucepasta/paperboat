package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The edge opens its browser-terminal control socket with its own credential and Go's default
// Accept-Encoding: gzip. Response compression must leave a WebSocket upgrade alone: since PR-03 no
// browser-terminal ticket was consumed on prod (the control socket upgraded, then carried nothing).
func TestCompressionLeavesAWebSocketUpgradeWorking(t *testing.T) {
	server := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"admission":"`+strings.Repeat("x", 2048)+`"}`)); err != nil {
			return
		}
		_, data, err := conn.Read(ctx)
		if err == nil {
			_ = conn.Write(ctx, websocket.MessageText, data)
		}
	})))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{"Authorization": {"Bearer edge"}, "Accept-Encoding": {"gzip"}}
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("upgrade through compression: %v", err)
	}
	defer conn.CloseNow()
	_, first, err := conn.Read(ctx)
	if err != nil || !strings.HasPrefix(string(first), `{"admission":"`) {
		t.Fatalf("first frame = %.40q, %v", first, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, echo, err := conn.Read(ctx); err != nil || string(echo) != "ping" {
		t.Fatalf("echo = %q, %v", echo, err)
	}
}
