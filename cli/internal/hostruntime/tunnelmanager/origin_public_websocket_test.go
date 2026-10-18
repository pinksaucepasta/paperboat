package tunnelmanager

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

// F7-05 device finding: a WebSocket through a public preview ended in 502. The
// public path (inspector enabled, disabled or saturated) must hand the 101 and
// the frames through to a real RFC 6455 echo origin.
func TestServePublicHTTPCarriesAWebSocketEchoInEveryCaptureMode(t *testing.T) {
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	accept := func() string {
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		return base64.StdEncoding.EncodeToString(sum[:])
	}()
	for _, mode := range []string{"disabled", "queue_full", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hijacker, ok := w.(http.Hijacker)
				if !ok || r.Header.Get("Upgrade") != "websocket" {
					http.Error(w, "not an upgrade", http.StatusBadRequest)
					return
				}
				connection, buffered, err := hijacker.Hijack()
				if err != nil {
					return
				}
				defer connection.Close()
				_, _ = io.WriteString(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
				_ = buffered.Flush()
				echo := make([]byte, 5)
				if _, err := io.ReadFull(buffered, echo); err == nil {
					_, _ = connection.Write(echo)
				}
			}))
			defer origin.Close()
			store, registry := inspector.NewStore(), inspector.NewRegistry()
			if err := store.SetPolicy("preview_ws", inspector.ResourcePolicy{Enabled: mode != "disabled"}); err != nil {
				t.Fatal(err)
			}
			if mode == "queue_full" {
				for i := 0; i < inspector.WorkQueueResourceMax; i++ {
					p, err := store.TryBegin("preview_ws")
					if err != nil {
						t.Fatal(err)
					}
					defer store.Abandon(p)
				}
			}
			transport := &OriginHTTPTransport{}
			defer transport.CloseIdleConnections()
			forwarder := OriginStreamForwarder{Transport: transport, Inspector: store, Registry: registry}
			route := inspectorTestRoute(origin.Listener.Addr().String())
			identity := CaptureIdentity{ResourceID: "preview_ws", ResourceGeneration: 1, RouteGeneration: 1, TargetGeneration: 1, ExpiresAt: time.Now().Add(time.Minute)}
			server, client := net.Pipe()
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				done <- forwarder.ServePublicHTTP(ctx, server, bufio.NewReader(server), route, identity)
			}()
			// The headers a standard Go WebSocket client sends.
			request := "GET /ws HTTP/1.1\r\nHost: preview.example.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\nUser-Agent: Go-http-client/1.1\r\n\r\n"
			if _, err := io.WriteString(client, request); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(client)
			response, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatalf("no upgrade response: %v (serve: %v)", err, <-done)
			}
			if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Sec-WebSocket-Accept") != accept {
				t.Fatalf("status=%d headers=%v", response.StatusCode, response.Header)
			}
			if _, err := client.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			echoed := make([]byte, 5)
			if _, err := io.ReadFull(reader, echoed); err != nil || !bytes.Equal(echoed, []byte("hello")) {
				t.Fatalf("echo=%q err=%v", echoed, err)
			}
			_ = client.Close()
			<-done
		})
	}
}
