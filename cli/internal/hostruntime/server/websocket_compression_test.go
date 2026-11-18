package server

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
)

// A websocket connection backs compression off for incompressible output exactly like the native one: the chunks are
// still delivered intact, and most skip the zstd attempt.
func TestWebSocketOutputSkipsCompressionOfIncompressibleChunks(t *testing.T) {
	random := make([]byte, 32<<10)
	rand.New(rand.NewSource(3)).Read(random)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		serverCtx, stop := context.WithCancel(ctx)
		defer stop()
		c := &webSocketConnection{connection: connection, ctx: serverCtx, cancel: stop}
		var err2 error
		for i := 0; i < 80 && err2 == nil; i++ {
			err2 = c.WriteTerminalOutput(3, protocol.BinaryFrame{Channel: protocol.TerminalStdout, StartSequence: uint64(i), Data: random})
		}
		done <- err2
	}))
	defer server.Close()
	client, _, err := websocket.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	client.SetReadLimit(protocol.MaxBinaryFrame + 64)
	before := protocol.TerminalCompressionMetrics()
	for i := 0; i < 80; i++ {
		_, message, err := client.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := protocol.DecodeTerminalOutput(message)
		if err != nil || len(frame.Data) != len(random) || frame.StreamID != 3 {
			t.Fatalf("frame %d: %v (%d bytes, stream %d)", i, err, len(frame.Data), frame.StreamID)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if skipped := protocol.TerminalCompressionMetrics().SkippedFrames - before.SkippedFrames; skipped < 40 {
		t.Fatalf("only %d of 80 incompressible chunks skipped the attempt", skipped)
	}
}
