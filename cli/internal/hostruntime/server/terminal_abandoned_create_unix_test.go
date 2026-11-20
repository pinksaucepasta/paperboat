//go:build linux || darwin

package server

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
)

// gatedLauncher holds each launch until the test releases it: a host too loaded to start a shell in time.
type gatedLauncher struct {
	SessionLauncher
	release chan struct{}
}

func (l gatedLauncher) Launch(ctx context.Context, request process.LaunchRequest) (session.Snapshot, error) {
	<-l.release
	return l.SessionLauncher.Launch(ctx, request)
}

// F21-164: a slow create finished after its requester (the control plane's create call, or a client) had gone, and
// the shell ran with no session record. A create whose answer cannot be delivered is closed again; a delivered one
// stays even though nobody attached it (the control plane creates every session before its client attaches).
func TestTerminalCreateClosedWhenItsAnswerCannotBeDelivered(t *testing.T) {
	server := verticalServerCommand(t, []string{"-c", "exec sleep 60"})
	dispatcher := server.config.Handler.(*Dispatcher)
	launcher := gatedLauncher{SessionLauncher: dispatcher.config.SessionLauncher, release: make(chan struct{})}
	dispatcher.config.SessionLauncher = launcher
	sessions := dispatcher.config.Sessions
	state := func(id string) session.State {
		snapshot, err := sessions.Snapshot(id)
		if err != nil {
			return session.State(err.Error()) // a close keeps the closed session: unknown is not closed
		}
		return snapshot.State
	}
	create := func(id string, wait bool) {
		client, peer := net.Pipe()
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		go server.Serve(peer)
		hello(t, client)
		payload, _ := json.Marshal(map[string]any{"action": "create", "session_id": id, "name": id, "columns": 80, "rows": 24, "existing_snapshot": true})
		if err := protocol.WriteFrame(client, request("req_"+id, "op_"+id, payload)); err != nil {
			t.Fatal(err)
		}
		if !wait {
			_ = client.Close() // the requester gives up before the shell starts
			launcher.release <- struct{}{}
			return
		}
		launcher.release <- struct{}{}
		if response, err := protocol.ReadFrame(client); err != nil || response.Type != "response" {
			t.Fatalf("create response=%+v err=%v", response, err)
		}
		_ = client.Close()
	}
	create("ses_undelivered", false)
	deadline := time.Now().Add(5 * time.Second)
	for state("ses_undelivered") != session.Closed && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := state("ses_undelivered"); got != session.Closed {
		t.Fatalf("create answered to a gone requester: session %s, want closed", got)
	}
	create("ses_delivered", true)
	time.Sleep(200 * time.Millisecond)
	if got := state("ses_delivered"); got != session.Running {
		snapshot, _ := sessions.Snapshot("ses_delivered")
		t.Fatalf("delivered create with nobody attached: session %s (exit %+v, participants %d), want running", got, snapshot.Exit, len(snapshot.Participants))
	}
}
