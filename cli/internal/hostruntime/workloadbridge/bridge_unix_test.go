//go:build darwin || linux

package workloadbridge

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestOwnerBridgePreservesRealShellChildReplayAndInputAcrossWorkerReplacement(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(c pty.Command) (session.PTYProcess, error) { return adapter.Start(c) }, TerminationGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, MaximumOperations: 4, ReplayBytes: 64 << 10, CancelGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer manager.Shutdown(context.Background())
	defer executions.Shutdown(context.Background())
	var mu sync.Mutex
	fence := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker-one", Epoch: 1, APIVersion: 1}
	token := make([]byte, 32)
	token[0] = 7
	server := &Server{Endpoint: filepath.Join(root, "socket", "owner.sock"), Token: token, Sessions: manager, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 4}, MaximumConcurrent: 8, Ready: make(chan error, 1), Fence: func() hostdproto.Status { mu.Lock(); defer mu.Unlock(); return fence }, AcquireActive: func(worker string, epoch uint64) (func(), error) {
		mu.Lock()
		if worker != fence.WorkerID || epoch != fence.Epoch {
			mu.Unlock()
			return nil, hostdproto.ErrFenced
		}
		return mu.Unlock, nil
	}}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client, err := NewClient(server.Endpoint, token, "worker-one", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, _ := client.Sessions()
	created, err := remote.Create(ctx, session.CreateRequest{ID: "session-preserved", Name: "preserved", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", `echo $$ > shell.pid; sleep 120 & echo $! > child.pid; while read line; do printf 'reply:%s\n' "$line"; done`}, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	participant := session.Participant{AttachmentID: "attach-one", AccountID: "account-one", ClientID: "client-one", Role: "owner", Browser: false, ConnectedAt: time.Now().UTC()}
	if _, err = remote.AttachLiveParticipantAtGeneration(created.ID, participant, created.Generation); err != nil {
		t.Fatal(err)
	}
	key := session.InputKey{ClientID: participant.ClientID, AttachmentID: participant.AttachmentID, Generation: created.Generation, InputSequence: 1}
	decision, err := remote.Write(created.ID, key, []byte("before\n"))
	if err != nil || decision.Status != session.InputAccepted {
		t.Fatalf("input=%+v err=%v", decision, err)
	}
	var output string
	for !strings.Contains(output, "reply:before") {
		event, err := remote.WaitNext(ctx, created.ID, participant.AttachmentID)
		if err != nil {
			t.Fatal(err)
		}
		output += string(event.Data)
		if err = remote.Acknowledge(created.ID, participant.AttachmentID, event.EndSequence); err != nil {
			t.Fatal(err)
		}
	}
	browser := participant
	browser.AttachmentID = "browser-one"
	browser.Browser = true
	if _, err = remote.AttachLiveParticipantAtGeneration(created.ID, browser, created.Generation); err != nil {
		t.Fatal(err)
	}
	if _, _, err = remote.BrowserScreenAttachment(created.ID, browser.AttachmentID, browser.AccountID, browser.ClientID, created.Generation); err != nil {
		t.Fatalf("browser flag lost: %v", err)
	}
	pids := make([]int, 0, 2)
	for _, name := range []string{"shell.pid", "child.pid"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	if err = remote.Shutdown(ctx); err != nil {
		t.Fatal(err)
	} // worker teardown cannot terminate owner
	mu.Lock()
	fence.WorkerID = "worker-two"
	fence.Epoch = 2
	mu.Unlock()
	replacement, _ := NewClient(server.Endpoint, token, "worker-two", 2)
	defer replacement.Close()
	next, _ := replacement.Sessions()
	if err = replacement.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = remote.Snapshot(created.ID); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale worker access=%v", err)
	}
	snapshot, err := next.Snapshot(created.ID)
	if err != nil || snapshot.Generation != created.Generation || snapshot.State != session.Running || len(snapshot.Participants) != 0 {
		t.Fatalf("preserved snapshot=%+v err=%v", snapshot, err)
	}
	for _, pid := range pids {
		if err = syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d lost: %v", pid, err)
		}
	}
	if stored, err := next.QueryInput(created.ID, key); err != nil || stored.Status != session.InputAccepted || stored.BytesWritten != decision.BytesWritten {
		t.Fatalf("input decision lost=%+v err=%v", stored, err)
	}
	attached, err := next.Attach(created.ID, "attach-two", 0)
	if err != nil {
		t.Fatal(err)
	}
	var replay string
	for _, event := range attached.Replay.Events {
		replay += string(event.Data)
	}
	if !strings.Contains(replay, "reply:before") {
		t.Fatalf("replay lost: %q", replay)
	}
	if _, err = next.CloseAtGeneration(ctx, created.ID, created.Generation); err != nil {
		t.Fatal(err)
	}
}
