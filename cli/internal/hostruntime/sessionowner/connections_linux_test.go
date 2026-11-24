package sessionowner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workloadbridge"
)

// ownerCatalog publishes a healthy owner (1) and one whose endpoint accepts and never answers (2).
func ownerCatalog(t testing.TB) (context.Context, *Connections, Descriptor, Descriptor) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned catalog fixture")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-owners-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	pin, runtimeRoot := filepath.Join(root, "pb"), filepath.Join(root, "runtime")
	if err := os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(pin, "qualification", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	catalog := Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: &absentOwnerRunner{}}
	healthy := Descriptor{Schema: Schema, Generation: generation(1), Instance: "lock-test", Source: source, Executable: pin, RuntimeRoot: runtimeRoot, Endpoint: filepath.Join(root, "a.sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	hung := healthy
	hung.Generation, hung.Endpoint = generation(2), filepath.Join(root, "b.sock")
	ctx, cancel := context.WithCancel(context.Background())
	for _, d := range []Descriptor{healthy, hung} {
		if err := catalog.Publish(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("unix", hung.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(hung.Endpoint, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(pty.Command) (session.PTYProcess, error) { return nil, session.ErrInvalidSession }})
	if err != nil {
		t.Fatal(err)
	}
	token, err := ReadCapability(healthy)
	if err != nil {
		t.Fatal(err)
	}
	server := &workloadbridge.Server{Endpoint: healthy.Endpoint, Token: token, Sessions: manager, SessionOnly: true, Identity: &workloadbridge.OwnerIdentity{Schema: Schema, Generation: healthy.Generation, ExecutableSHA256: source.SHA256}, MaximumConcurrent: 8, Ready: make(chan error, 1), Fence: func() hostdproto.Status {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: healthy.Generation, Epoch: 1}
	}, AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil }}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-done; _ = manager.Shutdown(context.Background()) })
	start := func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }
	connections, err := NewConnections(ctx, catalog, healthy, start, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connections.Close)
	return ctx, connections, healthy, hung
}

// Typing in one terminal must not wait for another generation's owner that does not answer.
func TestHungOwnerDoesNotDelayAnotherOwnersCalls(t *testing.T) {
	ctx, connections, healthy, hung := ownerCatalog(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	failed := make(chan error, 1)
	go func() { _, err := connections.ensure(ctx, hung.Generation); failed <- err }()
	time.Sleep(200 * time.Millisecond)
	for range 20 {
		started := time.Now()
		owner, err := connections.ensure(ctx, healthy.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Call(ctx, "List", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(started); waited > 500*time.Millisecond {
			t.Fatalf("call waited %v for another owner", waited)
		}
	}
	select {
	case err := <-failed:
		t.Fatalf("the hung owner answered early, so the test proved nothing: %v", err)
	default:
	}
	// A second caller of the hung owner stops at its own deadline.
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	started := time.Now()
	if _, err := connections.ensure(short, hung.Generation); err == nil || time.Since(started) > time.Second {
		t.Fatalf("waiting caller: %v after %v", err, time.Since(started))
	}
}

// The unread fast path must still notice a replaced descriptor and one that lost its custody.
func TestConnectedOwnerRechecksAChangedDescriptor(t *testing.T) {
	ctx, connections, healthy, _ := ownerCatalog(t)
	path := filepath.Join(connections.catalog.Root, healthy.Generation+".json")
	for range 2 {
		if _, err := connections.ensure(ctx, healthy.Generation); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := connections.ensure(ctx, healthy.Generation); err == nil {
		t.Fatal("a writable descriptor was accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	changed := healthy
	changed.Source.Version = "replaced"
	body, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(filepath.Dir(connections.catalog.Root), "staged")
	if err := os.WriteFile(staged, body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
	owner, err := connections.ensure(ctx, healthy.Generation)
	if err != nil || owner.Descriptor != changed {
		t.Fatalf("replaced descriptor not read: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := connections.ensure(ctx, healthy.Generation); err == nil {
		t.Fatal("a removed descriptor was accepted")
	}
}

// Opening an existing terminal must not wait for another one that is still starting.
func TestAttachDoesNotWaitForACreate(t *testing.T) {
	manager, workspace, shell := newRouterTestManager(t)
	gen := generation(91)
	owner := authenticatedRouterOwner(t, gen, manager, workspace, shell)
	call, creating, release := owner.Call, make(chan struct{}), make(chan struct{})
	owner.Call = func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		if op == "Create" && strings.Contains(string(body), "slow-create") {
			close(creating)
			<-release
		}
		return call(ctx, op, body)
	}
	router, err := NewRouter(gen, map[string]Owner{gen: {Descriptor: owner.Descriptor}}, 8, ensureRouterOwner(map[string]Owner{gen: owner}))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}
	request := func(name string) session.CreateRequest {
		return session.CreateRequest{Name: name, Command: routerShellCommand(shell, workspace, "exec sleep 60")}
	}
	created, err := remote.Create(context.Background(), request("ready"))
	if err != nil {
		t.Fatal(err)
	}
	slow := make(chan error, 1)
	go func() { _, err := remote.Create(context.Background(), request("slow-create")); slow <- err }()
	<-creating
	attached := make(chan error, 1)
	go func() { _, err := remote.AttachLive(created.ID, "viewer"); attached <- err }()
	select {
	case err = <-attached:
	case <-time.After(2 * time.Second):
		err = errors.New("attach waited for the create")
	}
	close(release)
	if err := errors.Join(err, <-slow); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkConnectedOwner(b *testing.B) {
	ctx, connections, healthy, _ := ownerCatalog(b)
	for b.Loop() {
		if _, err := connections.ensure(ctx, healthy.Generation); err != nil {
			b.Fatal(err)
		}
	}
}
