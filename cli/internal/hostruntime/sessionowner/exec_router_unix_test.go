//go:build darwin || linux

package sessionowner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	manager, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 4, MaximumOperations: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	_, client := execOwnerBridgeFor(t, ctx, root, generation, manager)
	return manager, client
}

// execOwnerBridgeFor serves executions on a real bridge socket under root and returns a client for it.
func execOwnerBridgeFor(t *testing.T, ctx context.Context, root, generation string, executions execprocess.Service) (execprocess.Service, *workloadbridge.Client) {
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
	token := make([]byte, 32)
	token[0] = 5
	var mu sync.Mutex
	fence := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1, APIVersion: 1}
	server := &workloadbridge.Server{Endpoint: filepath.Join(root, generation, "owner.sock"), Token: token, Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 4}, MaximumConcurrent: 8, Ready: make(chan error, 1), Fence: func() hostdproto.Status { mu.Lock(); defer mu.Unlock(); return fence }, AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil }}
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
	return executions, client
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

	unknown, _ := execprocess.EncodeMessage(map[string]string{"OperationID": "operation_nowhere"}, nil)
	if _, err := connections.execCall(ctx, "get", unknown); !errors.Is(err, execprocess.ErrNotFound) {
		t.Fatalf("a command no owner holds: %v", err)
	}
}

// failIfCalled is an owner behind a real bridge that fails the test when the router lets a call through.
type failIfCalled struct {
	execprocess.Service
	t *testing.T
}

func (f failIfCalled) Get(id string) (execprocess.ExecutionService, error) {
	f.t.Errorf("the router forwarded get %q to an owner", id)
	return nil, execprocess.ErrNotFound
}

func (f failIfCalled) Start(context.Context, execprocess.Request) (execprocess.ExecutionService, bool, error) {
	f.t.Error("the router forwarded start to an owner")
	return nil, false, execprocess.ErrInvalid
}

// The router answers what it can by itself: a body that is not a frame, a start without a request and the update
// fence never reach an owner.
func TestExecRouterRefusesMalformedCallsWithoutAskingAnOwner(t *testing.T) {
	root := shorttemp.Dir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, client := execOwnerBridgeFor(t, ctx, root, "guarded", failIfCalled{t: t})
	connections := &Connections{
		active:      Descriptor{Generation: "only_generation"},
		clients:     map[string]*workloadbridge.Client{"only_generation": client},
		descriptors: map[string]Descriptor{},
		execs:       map[string]string{"operation_guarded": "only_generation"},
	}
	if _, err := connections.execCall(ctx, "get", []byte(`{"OperationID":"operation_guarded"}`)); !errors.Is(err, execprocess.ErrInvalid) {
		t.Fatalf("a JSON body is not a frame: %v", err)
	}
	empty, _ := execprocess.EncodeMessage(struct{}{}, nil)
	if _, err := connections.execCall(ctx, "start", empty); !errors.Is(err, execprocess.ErrInvalid) {
		t.Fatalf("start without a request: %v", err)
	}
	if _, err := connections.execCall(ctx, "begin_update", empty); !errors.Is(err, execprocess.ErrUpdateUnavailable) {
		t.Fatalf("owner-held commands survive updates, nothing to fence: %v", err)
	}
	if _, err := connections.execCall(ctx, "list", []byte(`{`)); !errors.Is(err, execprocess.ErrInvalid) {
		t.Fatalf("list with a broken body: %v", err)
	}
}

// stubOwner is an owner whose active commands are synthetic: it lets one list answer outgrow a page.
type stubOwner struct {
	execprocess.Service
	snapshots []execprocess.Snapshot
}

func (s stubOwner) ActiveSnapshots() []execprocess.Snapshot { return s.snapshots }

// Several older owners hold commands across updates: a call for a command nobody remembers is probed through every
// live owner, active first, and the list joins them all, in pages.
func TestExecRouterProbesAndListsAcrossSeveralOlderOwners(t *testing.T) {
	root := shorttemp.Dir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	generations := []string{"active_generation", "older_one", "older_two", "older_three"}
	clients := map[string]*workloadbridge.Client{}
	managers := map[string]*execprocess.Manager{}
	for _, generation := range generations {
		manager, client := execOwnerBridge(t, ctx, root, generation)
		managers[generation], clients[generation] = manager, client
	}
	// A catalogued generation whose owner is gone: it is skipped, not fatal.
	generations = append(generations, "gone_generation")
	connections := &Connections{
		active:      Descriptor{Generation: "active_generation"},
		clients:     clients,
		descriptors: map[string]Descriptor{},
		execs:       map[string]string{},
		generations: func(context.Context) []string { return generations },
	}
	routed, err := connections.Executions()
	if err != nil {
		t.Fatal(err)
	}
	for _, generation := range generations[1:4] {
		if _, _, err := managers[generation].Start(ctx, execRequest(t, "operation_held_by_"+generation, root)); err != nil {
			t.Fatal(err)
		}
	}

	for _, generation := range generations[1:4] {
		id := "operation_held_by_" + generation
		execution, err := routed.Get(id)
		if err != nil {
			t.Fatalf("probe for %s: %v", id, err)
		}
		if snapshot := execution.Snapshot(); snapshot.OperationID != id {
			t.Fatalf("probe for %s answered %+v", id, snapshot)
		}
		if connections.execs[id] != generation {
			t.Fatalf("%s remembered on %q, want %q", id, connections.execs[id], generation)
		}
	}
	if _, err := routed.Get("operation_held_nowhere"); !errors.Is(err, execprocess.ErrNotFound) {
		t.Fatalf("a command no owner holds: %v", err)
	}

	listed := routed.ActiveSnapshots()
	if len(listed) != 3 {
		t.Fatalf("list across the older owners: %+v", listed)
	}
	seen := map[string]bool{}
	for _, snapshot := range listed {
		seen[snapshot.OperationID] = true
	}
	for _, generation := range generations[1:4] {
		if !seen["operation_held_by_"+generation] {
			t.Fatalf("list lacks the command of %s: %+v", generation, listed)
		}
	}
}

func TestExecRouterListIsPagedAcrossOwners(t *testing.T) {
	root := shorttemp.Dir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const perOwner = 1500 // three owners: 4500 snapshots, more than one page
	snapshots := func(owner string) []execprocess.Snapshot {
		out := make([]execprocess.Snapshot, perOwner)
		for i := range out {
			out[i] = execprocess.Snapshot{OperationID: fmt.Sprintf("operation_%s_%05d", owner, i), State: execprocess.StateRunning}
		}
		return out
	}
	clients := map[string]*workloadbridge.Client{}
	generations := []string{"active_generation", "older_one", "older_two"}
	for _, generation := range generations {
		_, client := execOwnerBridgeFor(t, ctx, root, generation, stubOwner{snapshots: snapshots(generation)})
		clients[generation] = client
	}
	connections := &Connections{
		active:      Descriptor{Generation: "active_generation"},
		clients:     clients,
		descriptors: map[string]Descriptor{},
		execs:       map[string]string{},
		generations: func(context.Context) []string { return generations },
	}
	type page struct {
		Snapshots []execprocess.Snapshot
		More      bool
	}
	var from uint64
	var got []execprocess.Snapshot
	pages := 0
	for {
		request, _ := execprocess.EncodeMessage(struct{ From uint64 }{from}, nil)
		frame, err := connections.execCall(ctx, "list", request)
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) > execprocess.MaxMessageBytes/4 {
			t.Fatalf("a list page is %d bytes", len(frame))
		}
		var p page
		if _, err := execprocess.DecodeMessage(frame, &p); err != nil {
			t.Fatal(err)
		}
		pages++
		got = append(got, p.Snapshots...)
		from += uint64(len(p.Snapshots))
		if !p.More {
			break
		}
		if pages > 10 {
			t.Fatal("paging does not end")
		}
	}
	if len(got) != 3*perOwner || pages < 2 {
		t.Fatalf("%d snapshots in %d pages, want %d in several", len(got), pages, 3*perOwner)
	}
	// Active owner first, then the older ones in the catalogue's order, each owner's list in its own order.
	for i, snapshot := range got {
		want := fmt.Sprintf("operation_%s_%05d", generations[i/perOwner], i%perOwner)
		if snapshot.OperationID != want {
			t.Fatalf("snapshot %d is %s, want %s", i, snapshot.OperationID, want)
		}
	}
	// And through the manager the worker uses: every page is read.
	routed, _ := connections.Executions()
	if all := routed.ActiveSnapshots(); len(all) != 3*perOwner {
		t.Fatalf("manager listed %d of %d", len(all), 3*perOwner)
	}
}
