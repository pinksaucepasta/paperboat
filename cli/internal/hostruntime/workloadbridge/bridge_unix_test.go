//go:build darwin || linux

package workloadbridge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"net"
	"net/http"
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
	root := shorttemp.Dir(t)
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
	// Idle terminal polling uses short caller deadlines. Those are ordinary
	// poll completion, not loss of the owner; subsequent input must still work.
	pollCtx, stopPoll := context.WithTimeout(ctx, 100*time.Millisecond)
	_, pollErr := remote.WaitNext(pollCtx, created.ID, participant.AttachmentID)
	stopPoll()
	if pollErr != context.DeadlineExceeded {
		t.Fatalf("idle poll became owner failure: %T %v", pollErr, pollErr)
	}
	cancelCtx, stopCanceled := context.WithCancel(ctx)
	stopCanceled()
	if _, err := remote.WaitNext(cancelCtx, created.ID, participant.AttachmentID); err != context.Canceled {
		t.Fatalf("canceled poll became owner failure: %T %v", err, err)
	}
	keyAfterPoll := key
	keyAfterPoll.InputSequence++
	if decision, err := remote.Write(created.ID, keyAfterPoll, []byte("after-poll\n")); err != nil || decision.Status != session.InputAccepted {
		t.Fatalf("input after idle poll=%+v err=%v", decision, err)
	}
	for !strings.Contains(output, "reply:after-poll") {
		event, err := remote.WaitNext(ctx, created.ID, participant.AttachmentID)
		if err != nil {
			t.Fatal(err)
		}
		output += string(event.Data)
		if err := remote.Acknowledge(created.ID, participant.AttachmentID, event.EndSequence); err != nil {
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

type bridgeFailureSessions struct {
	session.Service
	mu      sync.Mutex
	failure error
}

func (s *bridgeFailureSessions) Snapshot(string) (session.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return session.Snapshot{}, s.failure
}

func (s *bridgeFailureSessions) WaitNext(context.Context, string, string) (history.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return history.Event{}, s.failure
}

func TestOwnerBridgeDoesNotHideOperationalFailureBehindWireRejection(t *testing.T) {
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(pty.Command) (session.PTYProcess, error) { return nil, errors.New("unused launch") }})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: shorttemp.Dir(t), BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, MaximumOperations: 4, ReplayBytes: 64 << 10, CancelGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer executions.Shutdown(context.Background())
	reference := supportref.New()
	ctx, cancel := context.WithCancel(supportref.WithContext(context.Background(), reference))
	defer cancel()
	service := &bridgeFailureSessions{Service: manager, failure: session.ErrSessionUnknown}
	token := make([]byte, 32)
	server := &Server{Endpoint: filepath.Join(shorttemp.Dir(t), "socket", "owner.sock"), Token: token, Sessions: service, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 2}, MaximumConcurrent: 2, Ready: make(chan error, 1), Fence: func() hostdproto.Status {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1}
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
	client, err := NewClient(server.Endpoint, token, "worker", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, err := client.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { mu.Lock(); faults = append(faults, f); mu.Unlock() })
	defer restore()
	if _, err := remote.Snapshot("missing"); !errors.Is(err, session.ErrSessionUnknown) {
		t.Fatalf("pure rejection changed: %v", err)
	}
	mu.Lock()
	count := len(faults)
	mu.Unlock()
	if count != 0 {
		t.Fatal("pure expected rejection observed as operational failure")
	}
	for _, cause := range []error{session.ErrSessionUnknown, context.DeadlineExceeded} {
		service.mu.Lock()
		service.failure = errors.Join(cause, syscall.EIO)
		service.mu.Unlock()
		requestCtx, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
		if _, err := remote.WaitNext(requestCtx, "missing", "attachment"); !errors.Is(err, session.ErrOwnerUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			stop()
			t.Fatalf("mixed failure was hidden or retried: %v", err)
		}
		stop()
		mu.Lock()
		count = len(faults)
		last := errorreport.Fault{}
		if count > 0 {
			last = faults[count-1]
		}
		mu.Unlock()
		if count != 1 || last.Errno != int(syscall.EIO) || last.SupportReference != reference {
			t.Fatalf("original fault was not observed once with process identity: %#v count=%d", last, count)
		}
		mu.Lock()
		faults = nil
		mu.Unlock()
	}
}

func TestOwnerBridgeClientKeepsLocalTransportAndParserCausesPrivate(t *testing.T) {
	endpoint := filepath.Join(shorttemp.Dir(t), "socket", "owner.sock")
	client, err := NewClient(endpoint, make([]byte, 32), "worker", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Health(context.Background()); err == nil {
		t.Fatal("missing owner socket accepted")
	} else {
		var opErr *net.OpError
		if !errors.Is(err, session.ErrOwnerUnavailable) || !errors.As(err, &opErr) || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transport cause lost: %T", err)
		}
		if strings.Contains(err.Error(), endpoint) {
			t.Fatal("private socket path exposed")
		}
	}
	listener, err := hostdproto.ListenWorkloads(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer hostdproto.RemoveWorkloadSocket(endpoint)
	var mu sync.Mutex
	malformed := true
	responseStatus := 200
	oversized := false
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bad, status, large := malformed, responseStatus, oversized
		mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("private-bridge-marker"))
			return
		}
		if large {
			_, _ = w.Write([]byte(strings.Repeat("x", MaxBodyBytes+1)))
			return
		}
		if bad {
			_, _ = w.Write([]byte(`{"Data": @"private-bridge-marker"}`))
			return
		}
		_, _ = w.Write([]byte(`{"Data":{}}`))
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	err = client.Health(context.Background())
	var syntaxErr *json.SyntaxError
	if !errors.Is(err, session.ErrOwnerUnavailable) || !errors.As(err, &syntaxErr) {
		t.Fatalf("parser cause lost: %T", err)
	}
	if strings.Contains(err.Error(), "private-bridge-marker") {
		t.Fatal("private parser value exposed")
	}
	mu.Lock()
	malformed = false
	mu.Unlock()
	for _, test := range []struct {
		status             int
		large              bool
		cause, kind, stage string
	}{{403, false, "permission_denied", "ownerHTTPFailure", "target_connect"}, {503, false, "service_unavailable", "ownerHTTPFailure", "target_connect"}, {500, false, "upstream_failure", "ownerHTTPFailure", "target_connect"}, {200, true, "resource_exhausted", "ownerResponseLimitFailure", "delivery"}} {
		mu.Lock()
		responseStatus = test.status
		oversized = test.large
		mu.Unlock()
		err := client.Health(context.Background())
		if !errors.Is(err, session.ErrOwnerUnavailable) || err.Error() != session.ErrOwnerUnavailable.Error() {
			t.Fatalf("private response error changed public contract: %T", err)
		}
		fault := errorreport.ProjectFault(context.Background(), "paperboat-daemon", "sessions", "command", "terminal_session_failed", err)
		expectedStatus := test.status
		if test.large {
			expectedStatus = 0
		}
		if fault.Cause != test.cause || fault.ErrorType != test.kind || fault.HTTPStatus != expectedStatus || fault.Stage != test.stage {
			t.Fatalf("private response diagnostics lost: %#v", fault)
		}
	}
	mu.Lock()
	responseStatus = 200
	oversized = false
	mu.Unlock()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := client.Health(canceled); err != context.Canceled {
		t.Fatalf("owner cancellation lost: %T", err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("healthy owner did not recover: %v", err)
	}
}

// A transport can report cancellation together with an independent native
// failure. Cancellation must not hide that fault at the client boundary.
type bridgeFaultTransport struct{}

func (bridgeFaultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.Join(r.Context().Err(), syscall.EIO)
}

func TestOwnerBridgeCallerCancellationKeepsJoinedNativeFailure(t *testing.T) {
	client, err := NewClient(filepath.Join(shorttemp.Dir(t), "owner.sock"), make([]byte, 32), "worker", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.http.Transport = bridgeFaultTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Health(ctx); !errors.Is(err, session.ErrOwnerUnavailable) || !errors.Is(err, syscall.EIO) || !errors.Is(err, context.Canceled) {
		t.Fatalf("joined transport fault lost: %T %v", err, err)
	}
}
