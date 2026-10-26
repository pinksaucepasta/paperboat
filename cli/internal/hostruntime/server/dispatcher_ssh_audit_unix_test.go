//go:build darwin || linux

package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// F5-12: an SSH stream served through the dispatcher is audited with who opened it, the target generation and
// the bytes each way, from the same host that serves every other path.
func TestSSHDispatcherAuditsTheSessionWithBytes(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
		}
	}()
	var mu sync.Mutex
	var events []managedssh.SessionEvent
	host, err := managedssh.NewHost(managedssh.HostConfig{MaxStreams: 2, ProbeTimeout: time.Second, DialTimeout: time.Second, Audit: func(event managedssh.SessionEvent) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.ReconcileTarget(context.Background(), 4, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := execDispatcher(t)
	dispatcher.config.SSH = host
	authorization := Authorization{AccountID: "user_a", ClientID: "cli_1"}
	payload := json.RawMessage(`{"operation_id":"operation_ssh_0002","generation":4}`)
	outcome := dispatcher.HandleOperation(context.Background(), authorization, "ssh.v1", "operation_ssh_0002", payload)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, opened, err := dispatcher.OpenStream(ctx, authorization, "ssh.v1", payload, outcome, false)
	if err != nil || !opened {
		t.Fatalf("opened=%v err=%v", opened, err)
	}
	defer stream.Close()
	if err := dispatcher.HandleSSHInput(ctx, authorization, "operation_ssh_0002", []byte("opaque-ssh-bytes")); err != nil {
		t.Fatal(err)
	}
	if frame, err := stream.Next(ctx); err != nil || string(frame.Data) != "opaque-ssh-bytes" {
		t.Fatalf("frame=%#v err=%v", frame, err)
	}
	if err := dispatcher.HandleSSHEOF(ctx, authorization, "operation_ssh_0002"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		count := len(events)
		mu.Unlock()
		if count >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	start, end := events[0], events[1]
	wantPeer := managedssh.Peer{AccountID: "user_a", EndpointID: "cli_1", OperationID: "operation_ssh_0002"}
	if start.Kind != managedssh.SessionStarted || start.Peer != wantPeer || start.Generation != 4 {
		t.Fatalf("start = %+v", start)
	}
	if end.Kind != managedssh.SessionEnded || end.SessionID != start.SessionID || end.BytesIn != 16 || end.BytesOut != 16 || end.Outcome != managedssh.EndedClosed {
		t.Fatalf("end = %+v", end)
	}
}
