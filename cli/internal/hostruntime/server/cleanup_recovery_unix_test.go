//go:build linux || darwin

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestTerminalCleanupRetriesPastTransientJournalOutcome(t *testing.T) {
	dispatcher, root := execDispatcher(t)
	created, err := dispatcher.config.Sessions.Create(t.Context(), session.CreateRequest{Name: "cleanup", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "exec sleep 60"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	unavailable.Store(true)
	server := testServer(t, func(context.Context, protocol.Frame) (Authorization, error) {
		return Authorization{ClientID: "cleanup-cli", JournalBinding: "cleanup-owner", SessionID: created.ID, TerminalRole: TerminalRoleOwner}, nil
	}, func(ctx context.Context, a Authorization, cap string, body json.RawMessage) operation.Outcome {
		if unavailable.Load() {
			return operation.Outcome{ErrorCode: "unavailable"}
		}
		return dispatcher.Handle(ctx, a, cap, body)
	}, 2)
	client, peer := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	go server.Serve(peer)
	hello(t, client)
	for _, action := range []string{"close", "delete"} {
		payload, _ := json.Marshal(map[string]string{"action": action, "session_id": created.ID})
		unavailable.Store(true)
		first := sendRequest(t, client, request("first_"+action, "cleanup_"+action, payload))
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(first.Payload, &failure)
		if first.Type != "error" || failure.Code != "unavailable" {
			t.Fatalf("expected transient failure before %s, got %s", action, first.Type)
		}
		unavailable.Store(false)
		for i := 0; i < 2; i++ {
			response := sendRequest(t, client, request(fmt.Sprintf("retry_%s_%d", action, i), "cleanup_"+action, payload))
			if response.Type != "response" {
				t.Fatalf("%s retry replayed transient failure: %s", action, response.Type)
			}
		}
	}
	if _, err := dispatcher.config.Sessions.Snapshot(created.ID); err != session.ErrSessionUnknown {
		t.Fatalf("cleanup retained session: %v", err)
	}
}
