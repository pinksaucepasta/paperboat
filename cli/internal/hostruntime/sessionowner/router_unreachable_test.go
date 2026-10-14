//go:build linux

package sessionowner

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// An older generation whose owner cannot be started must not take terminal
// service away from the active generation.
func TestRouterServesActiveGenerationWhenPreviousOwnerCannotStart(t *testing.T) {
	activeManager, activeWorkspace, shell := newRouterTestManager(t)
	_, oldWorkspace, _ := newRouterTestManager(t)
	oldGeneration, activeGeneration := generation(61), generation(62)
	activeOwner := authenticatedRouterOwner(t, activeGeneration, activeManager, activeWorkspace, shell)
	metadata := map[string]Owner{
		oldGeneration:    {Descriptor: testOwnerDescriptor(t, oldGeneration, oldWorkspace, shell)},
		activeGeneration: {Descriptor: activeOwner.Descriptor},
	}
	ensure := func(ctx context.Context, gen string) (Owner, error) {
		if gen == oldGeneration {
			return Owner{}, &os.PathError{Op: "dial", Path: "/missing/control.sock", Err: os.ErrNotExist}
		}
		return ensureRouterOwner(map[string]Owner{activeGeneration: activeOwner})(ctx, gen)
	}
	router, err := NewRouter(activeGeneration, metadata, 4, 2, ensure)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}
	created, err := remote.Create(context.Background(), session.CreateRequest{Name: "after-dead-owner", Command: routerShellCommand(shell, activeWorkspace, "exec sleep 60")})
	if err != nil || created.ID == "" {
		t.Fatalf("create with unreachable previous owner: %#v err=%v", created, err)
	}
	listed := remote.List()
	if err := remote.LastError(); err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("list=%#v err=%v", listed, err)
	}
	if snapshot, err := remote.Snapshot(created.ID); err != nil || snapshot.ID != created.ID {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

// Closed records retained by retired generations cannot hold a process and
// must not exhaust the live-session limit for new terminals.
func TestRouterAdmitsNewSessionsWhenRetiredGenerationsRetainClosedRecords(t *testing.T) {
	activeManager, activeWorkspace, shell := newRouterTestManager(t)
	_, oldWorkspace, _ := newRouterTestManager(t)
	oldGeneration, activeGeneration := generation(71), generation(72)
	activeOwner := authenticatedRouterOwner(t, activeGeneration, activeManager, activeWorkspace, shell)
	metadata := map[string]Owner{
		oldGeneration:    {Descriptor: testOwnerDescriptor(t, oldGeneration, oldWorkspace, shell)},
		activeGeneration: {Descriptor: activeOwner.Descriptor},
	}
	router, err := NewRouter(activeGeneration, metadata, 2, 2, ensureRouterOwner(map[string]Owner{activeGeneration: activeOwner}))
	if err != nil {
		t.Fatal(err)
	}
	router.archive = func(_ context.Context, gen string) (workloadbridge.ArchiveMetadata, error) {
		return workloadbridge.ArchiveMetadata{Available: true, Generation: gen, Snapshots: []session.Snapshot{
			{ID: "terminal_closed_one", Name: "closed-one", State: session.Closed},
			{ID: "terminal_closed_two", Name: "closed-two", State: session.Closed},
		}}, nil
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"live-one", "live-two"} {
		if _, err := remote.Create(context.Background(), session.CreateRequest{Name: name, Command: routerShellCommand(shell, activeWorkspace, "exec sleep 60")}); err != nil {
			t.Fatalf("create %s beside retired closed records: %v", name, err)
		}
	}
	if _, err := remote.Create(context.Background(), session.CreateRequest{Name: "live-three", Command: routerShellCommand(shell, activeWorkspace, "exec sleep 60")}); !errors.Is(err, session.ErrResourceLimit) {
		t.Fatalf("live-session limit not enforced: %v", err)
	}
}
