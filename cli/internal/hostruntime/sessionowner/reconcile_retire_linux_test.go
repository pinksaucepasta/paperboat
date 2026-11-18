//go:build linux

package sessionowner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workloadbridge"
)

// After an update the previous owner is retired by the updater's reconcile pass: a running owner that holds no
// live session is stopped and forgotten; one that still serves a live session stays untouched until that
// session ends, and the next pass then retires it (no orphan, and no shell cut).
func TestReconcileRetiresAnOwnerOnceItHoldsNoLiveSession(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned catalog fixture")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-retire-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "pb")
	if err = os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(pin, "qualification", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if err = os.Mkdir(runtimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	runner := &retiringOwnerRunner{}
	catalog := Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: runner}
	old := Descriptor{Schema: Schema, Generation: generation(1), Instance: "retire-test", Source: source, Executable: pin, RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(root, "old-run", "control.sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	active := old
	active.Generation, active.InitialStore, active.Endpoint = generation(2), false, filepath.Join(root, "active.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, d := range []Descriptor{old, active} {
		if err = catalog.Publish(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	installer, err := catalog.installer(old, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = installer.PublishDefinition(ctx); err != nil {
		t.Fatal(err)
	}
	backdate(t, catalog, old)
	if err = os.Mkdir(filepath.Dir(old.Endpoint), 0700); err != nil {
		t.Fatal(err)
	}
	token, err := ReadCapability(old)
	if err != nil {
		t.Fatal(err)
	}
	manager, workspace, shell := newRouterTestManager(t)
	server := &workloadbridge.Server{Endpoint: old.Endpoint, Token: token, Sessions: manager, SessionOnly: true,
		Identity:          &workloadbridge.OwnerIdentity{Schema: Schema, Generation: old.Generation, ExecutableSHA256: source.SHA256},
		MaximumConcurrent: 8, Ready: make(chan error, 1),
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: old.Generation, Epoch: 1}
		},
		AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil },
		Retire:        manager.Retire}
	serverCtx, stopServer := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Run(serverCtx) }()
	if err = <-server.Ready; err != nil {
		t.Fatal(err)
	}
	defer func() { stopServer(); <-done }()

	created, err := manager.Create(ctx, session.CreateRequest{Name: "pre-update-shell", Command: routerShellCommand(shell, workspace, "exec sleep 600")})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := Coordinator{Catalog: catalog, Active: active}
	if err = coordinator.Reconcile(ctx); err != nil {
		t.Fatalf("a busy owner is not an error: %v", err)
	}
	if runner.removals != 0 {
		t.Fatal("an owner serving a live session was removed")
	}
	if _, err = Find(catalog.Root, old.Generation); err != nil {
		t.Fatalf("a busy owner left the catalog: %v", err)
	}

	if _, err = manager.Close(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err = waitSessionEnded(ctx, manager, created.ID); err != nil {
		t.Fatal(err)
	}
	if err = coordinator.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.removals == 0 {
		t.Fatal("the owner kept running after its last live session ended")
	}
	if _, err = Find(catalog.Root, active.Generation); err != nil {
		t.Fatalf("the active owner left the catalog: %v", err)
	}
	if _, err = Find(catalog.Root, old.Generation); err == nil || !errors.Is(err, os.ErrNotExist) {
		// a retained (stored) session keeps the catalogued archive, never a process
		if err != nil {
			t.Fatalf("finding the retired owner: %v", err)
		}
	}
}

func waitSessionEnded(ctx context.Context, manager *session.Manager, id string) error {
	for {
		for _, s := range manager.List() {
			if s.ID == id && (s.State == session.Exited || s.State == session.Closed) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// retiringOwnerRunner reports the owner's unit running until systemd is told to disable it, then gone.
type retiringOwnerRunner struct{ runningOwnerRunner }

func (r *retiringOwnerRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	if r.removals > 0 {
		return "LoadState=not-found\n", nil
	}
	return r.runningOwnerRunner.Output(ctx, name, args...)
}
