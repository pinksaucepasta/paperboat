package preview

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

// F7-05: the edge writes the GET upgrade onto a carrier stream and then keeps it
// open for the upgraded bytes. After the 101 the host must pipe both directions
// until a side closes, with public capture enabled and without it.
func TestDataCarrierPreviewCarrierPipesAWebSocketAfterTheUpgrade(t *testing.T) {
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	for _, mode := range []string{"raw", "captured"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer connection.Close()
				_, _ = io.WriteString(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
				_ = buffered.Flush()
				for {
					frame := make([]byte, 5)
					if _, err := io.ReadFull(buffered, frame); err != nil {
						return
					}
					if _, err := connection.Write(frame); err != nil {
						return
					}
				}
			}))
			defer origin.Close()
			identity := testPreviewCarrierIdentity(1)
			pair := newPreviewCarrierPair(t, ctx, identity)
			defer pair.close()
			hub, err := NewDataCarrierPreviewHub(ctx, DataCarrierPreviewHubConfig{Active: pair.active, Identity: identity})
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			config := DataCarrierPreviewCarrierConfig{Hub: hub, Identity: identity, RouteID: "pvc_rte_ws", DialOrigin: func(ctx context.Context, target LeaseTarget) (io.ReadWriteCloser, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", target.Address)
			}}
			lease := Lease{ID: "preview-ws", AccountID: identity.AccountID, Generation: 2, AccessMode: "public", Endpoint: "https://public.example.test", Target: LeaseTarget{Scheme: "http", Address: origin.Listener.Addr().String()}, LeaseDeadline: time.Now().UTC().Add(time.Hour)}
			if mode == "captured" {
				store, registry := inspector.NewStore(), inspector.NewRegistry()
				if err := store.SetPolicy(lease.ID, inspector.ResourcePolicy{Enabled: true}); err != nil {
					t.Fatal(err)
				}
				config.Inspector, config.Registry = store, registry
			}
			carrier, err := NewDataCarrierPreviewCarrier(config)
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan struct{})
			go func() { _ = carrier.Run(ctx, lease, func(Lease) error { close(ready); return nil }) }()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("carrier not ready")
			}
			stream, err := pair.edge.OpenStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			open := testPreviewStreamOpen(identity, "pvc_rte_ws", "request-ws")
			open.Kind = "websocket"
			if err := connectorprotocol.WriteStreamOpen(stream, open); err != nil {
				t.Fatal(err)
			}
			_, _ = fmt.Fprintf(stream, "GET /ws HTTP/1.1\r\nHost: public.example.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", key)
			reader := bufio.NewReader(stream)
			response, err := http.ReadResponse(reader, nil)
			if err != nil || response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Sec-WebSocket-Accept") != accept {
				t.Fatalf("upgrade response=%v err=%v", response, err)
			}
			for round := 0; round < 3; round++ {
				payload := []byte(fmt.Sprintf("hi-%02d", round))
				if _, err := stream.Write(payload); err != nil {
					t.Fatalf("round %d write: %v", round, err)
				}
				echoed := make([]byte, len(payload))
				if _, err := io.ReadFull(reader, echoed); err != nil || string(echoed) != string(payload) {
					t.Fatalf("round %d echo=%q err=%v", round, echoed, err)
				}
			}
		})
	}
}
