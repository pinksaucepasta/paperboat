package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// Requests queued on a connection reach startRequest from untracked frame workers; one that arrives while Shutdown
// waits must be refused, never added to the WaitGroup Wait is already draining (go test -race reports that Add).
func TestRequestAdmissionConcurrentShutdown(t *testing.T) {
	for round := 0; round < 50; round++ {
		var started, afterShutdown atomic.Int32
		var shutDown atomic.Bool
		server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
			return Authorization{JournalBinding: "user:1"}, nil
		}, func(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
			started.Add(1)
			if shutDown.Load() {
				afterShutdown.Add(1)
			}
			return operation.Outcome{}
		}, 64)
		client, peer := net.Pipe()
		go io.Copy(io.Discard, client)
		writer := newLockedWriter(peer)
		var senders sync.WaitGroup
		for i := 0; i < 16; i++ {
			senders.Add(1)
			go func() {
				defer senders.Done()
				for j := 0; j < 8; j++ {
					server.startRequest(request("req", "op_0000000"+string(rune('a'+i)), json.RawMessage(`{}`)), writer, server.config.Authorizer, context.Background(), peer, newTerminalConnectionState())
				}
			}()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("round %d: shutdown: %v", round, err)
		}
		cancel()
		shutDown.Store(true)
		senders.Wait()
		time.Sleep(time.Millisecond)
		if afterShutdown.Load() != 0 {
			t.Fatalf("round %d: %d requests ran after Shutdown returned", round, afterShutdown.Load())
		}
		writer.close()
		client.Close()
		peer.Close()
	}
}
