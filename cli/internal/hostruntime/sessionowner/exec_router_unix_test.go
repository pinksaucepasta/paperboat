//go:build darwin || linux

package sessionowner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
)

// execOwnerBridge serves one owner's executions on a real bridge socket and returns the manager behind it and a client.
func execOwnerBridge(t *testing.T, ctx context.Context, root, generation string) (*execprocess.Manager, *workloadbridge.Client) {
	t.Helper()
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(c pty.Command) (session.PTYProcess, error) { return adapter.Start(c) }, TerminationGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })
	manager, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 4, MaximumOperations: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	token := make([]byte, 32)
	token[0] = 5
	var mu sync.Mutex
	fence := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1, APIVersion: 1}
	server := &workloadbridge.Server{Endpoint: filepath.Join(root, generation, "owner.sock"), Token: token, Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: manager, MaximumReaders: 4}, MaximumConcurrent: 8, Ready: make(chan error, 1), Fence: func() hostdproto.Status { mu.Lock(); defer mu.Unlock(); return fence }, AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil }}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	client, err := workloadbridge.NewClient(server.Endpoint, token, "worker", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return manager, client
}

func execRequest(t *testing.T, id string, root string) execprocess.Request {
	t.Helper()
	return execprocess.Request{OperationID: id, Argv: []string{"/bin/sh", "-c", "sleep 30"}, CWD: root}
}

// Commands run in the owner that was active when they started and stay there across updates; the router sends every
// call for one to that owner, over the framed bridge.
func TestExecRouterSendsCallsToTheOwnerHoldingTheCommand(t *testing.T) {
	root := shorttemp.Dir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	older, olderClient := execOwnerBridge(t, ctx, root, "old")
	active, activeClient := execOwnerBridge(t, ctx, root, "new")
	connections := &Connections{
		active:      Descriptor{Generation: "new_generation"},
		clients:     map[string]*workloadbridge.Client{"old_generation": olderClient, "new_generation": activeClient},
		descriptors: map[string]Descriptor{},
		execs:       map[string]string{},
	}
	routed, err := connections.Executions()
	if err != nil {
		t.Fatal(err)
	}

	// A command that started on the older owner (its ID is remembered) keeps being served by it.
	if _, _, err := older.Start(ctx, execRequest(t, "operation_on_old_owner", root)); err != nil {
		t.Fatal(err)
	}
	connections.rememberExec("operation_on_old_owner", "old_generation")
	execution, err := routed.Get("operation_on_old_owner")
	if err != nil {
		t.Fatalf("get of a command on the older owner: %v", err)
	}
	if snapshot := execution.Snapshot(); snapshot.OperationID != "operation_on_old_owner" || snapshot.State == execprocess.StateExited {
		t.Fatalf("snapshot %+v", snapshot)
	}

	// A new command starts on the active owner and is remembered there.
	started, _, err := routed.Start(ctx, execRequest(t, "operation_on_new_owner", root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := active.Get("operation_on_new_owner"); err != nil {
		t.Fatalf("start did not reach the active owner: %v", err)
	}
	if _, err := older.Get("operation_on_new_owner"); !errors.Is(err, execprocess.ErrNotFound) {
		t.Fatalf("start reached the older owner: %v", err)
	}
	if connections.execs["operation_on_new_owner"] != "new_generation" {
		t.Fatalf("start not remembered: %v", connections.execs)
	}
	if _, err := started.WaitStarted(ctx); err != nil {
		t.Fatal(err)
	}

	// With nothing remembered the router probes the live owners (here only the active one: the catalogue is empty) and
	// finds the command there, and remembers it.
	connections.execs = map[string]string{}
	if _, err := routed.Get("operation_on_new_owner"); err != nil {
		t.Fatalf("probe for a forgotten route: %v", err)
	}
	if connections.execs["operation_on_new_owner"] != "new_generation" {
		t.Fatalf("a probe hit was not remembered: %v", connections.execs)
	}
}

func TestExecRouterListsAcrossOwnersAndRefusesWhatIsNotAFrame(t *testing.T) {
	root := shorttemp.Dir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	older, olderClient := execOwnerBridge(t, ctx, root, "old")
	_, activeClient := execOwnerBridge(t, ctx, root, "new")
	connections := &Connections{
		active:      Descriptor{Generation: "new_generation"},
		clients:     map[string]*workloadbridge.Client{"old_generation": olderClient, "new_generation": activeClient},
		descriptors: map[string]Descriptor{},
		execs:       map[string]string{},
	}
	routed, err := connections.Executions()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := routed.Start(ctx, execRequest(t, "operation_listed_new", root)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := older.Start(ctx, execRequest(t, "operation_listed_old", root)); err != nil {
		t.Fatal(err)
	}
	// Only the active owner is listed here: the catalogue is empty, so older generations are not enumerated.
	listed := routed.ActiveSnapshots()
	if len(listed) != 1 || listed[0].OperationID != "operation_listed_new" {
		t.Fatalf("list %+v, want the active owner's one command", listed)
	}

	if _, err := connections.execCall(ctx, "get", []byte(`{"OperationID":"operation_listed_new"}`)); !errors.Is(err, execprocess.ErrInvalid) {
		t.Fatalf("a JSON body is not a frame: %v", err)
	}
	empty, _ := execprocess.EncodeMessage(struct{}{}, nil)
	if _, err := connections.execCall(ctx, "start", empty); !errors.Is(err, execprocess.ErrInvalid) {
		t.Fatalf("start without a request: %v", err)
	}
	if _, err := connections.execCall(ctx, "begin_update", empty); !errors.Is(err, execprocess.ErrUpdateUnavailable) {
		t.Fatalf("owner-held commands survive updates, nothing to fence: %v", err)
	}
	unknown, _ := execprocess.EncodeMessage(map[string]string{"OperationID": "operation_nowhere"}, nil)
	if _, err := connections.execCall(ctx, "get", unknown); !errors.Is(err, execprocess.ErrNotFound) {
		t.Fatalf("a command no owner holds: %v", err)
	}
}

// execList joins every catalogued owner's active commands into one message: at the catalogue's size and the default
// per-owner command limit it must fit the message cap with room to spare.
func TestExecListAcrossAFullCatalogueFitsTheMessageCap(t *testing.T) {
	started := time.Now()
	worst := execprocess.Snapshot{OperationID: strings.Repeat("a", 128), State: execprocess.StateRunning, StartedAt: &started, ErrorCode: "e", NextSequence: 1<<64 - 1}
	var all struct{ Snapshots []execprocess.Snapshot }
	for range MaxCatalogEntries * runtimeconfig.DefaultResources.MaxConcurrentOps {
		all.Snapshots = append(all.Snapshots, worst)
	}
	message, err := execprocess.EncodeMessage(all, nil)
	if err != nil || len(message) > execprocess.MaxMessageBytes*3/4 {
		t.Fatalf("a full catalogue lists %d bytes (err %v), want under %d", len(message), err, execprocess.MaxMessageBytes*3/4)
	}
}
