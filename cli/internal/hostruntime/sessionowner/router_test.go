//go:build linux

package sessionowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

func TestRouterKeepsOldSessionsWithTheirOwnerAndAdmitsNewSessionsGlobally(t *testing.T) {
	oldManager, oldWorkspace, shell := newRouterTestManager(t)
	newManager, newWorkspace, _ := newRouterTestManager(t)
	oldGeneration, newGeneration := generation(1), generation(2)
	oldOwner := authenticatedRouterOwner(t, oldGeneration, oldManager, oldWorkspace, shell)
	newOwner := authenticatedRouterOwner(t, newGeneration, newManager, newWorkspace, shell)
	available := map[string]Owner{oldGeneration: oldOwner, newGeneration: newOwner}
	metadata := map[string]Owner{
		oldGeneration: {Descriptor: oldOwner.Descriptor},
	}
	router, err := NewRouter(oldGeneration, metadata, 2, ensureRouterOwner(available))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}

	oldSession, err := remote.Create(context.Background(), session.CreateRequest{
		Name:    "old-session",
		Command: routerShellCommand(shell, oldWorkspace, `printf 'old-ready'; IFS= read -r input; printf 'old:%s' "$input"; exec sleep 60`),
	})
	if err != nil {
		t.Fatalf("create old session: %v", err)
	}

	metadata[newGeneration] = Owner{Descriptor: newOwner.Descriptor}
	if err := router.SetOwners(newGeneration, metadata); err != nil {
		t.Fatalf("switch active generation: %v", err)
	}

	if snapshot, err := remote.Snapshot(oldSession.ID); err != nil || snapshot.ID != oldSession.ID || snapshot.Name != "old-session" {
		t.Fatalf("old session snapshot routed to its owner: snapshot=%#v err=%v", snapshot, err)
	}
	attached, err := remote.AttachLive(oldSession.ID, "att_router")
	if err != nil {
		t.Fatalf("attach to old owner: %v", err)
	}
	input := session.InputKey{ClientID: "client", AttachmentID: "att_router", Generation: oldSession.Generation, InputID: "input-router"}
	if decision, err := remote.Write(oldSession.ID, input, []byte("from-router\n")); err != nil || decision.Status != session.InputAccepted {
		t.Fatalf("write to old owner: decision=%#v err=%v", decision, err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output []byte
	for !bytes.Contains(output, []byte("old:from-router")) {
		event, err := remote.WaitNext(waitCtx, oldSession.ID, "att_router")
		if err != nil {
			t.Fatalf("read old owner output (attached at %d): %q: %v", attached.Replay.ToSequence, output, err)
		}
		output = append(output, event.Data...)
	}

	newSession, err := remote.Create(context.Background(), session.CreateRequest{
		Name:    "new-session",
		Command: routerShellCommand(shell, newWorkspace, `printf 'new-ready'; exec sleep 60`),
	})
	if err != nil {
		t.Fatalf("create in active owner: %v", err)
	}
	if newManager.List()[0].ID != newSession.ID || oldManager.List()[0].ID != oldSession.ID {
		t.Fatal("create or route changed the generation that owns a session")
	}
	// Attachments are bounded per session by the session manager, not across the machine, so a
	// second session takes its own attachment while another session already has one.
	if _, err := remote.AttachLive(newSession.ID, "att_second_session"); err != nil {
		t.Fatalf("attachment to a second session: %v", err)
	}

	listed := remote.List()
	if err := remote.LastError(); err != nil || len(listed) != 2 {
		t.Fatalf("aggregate owner list: sessions=%#v err=%v", listed, err)
	}
	listedIDs := map[string]string{}
	for _, snapshot := range listed {
		listedIDs[snapshot.ID] = snapshot.Name
	}
	if listedIDs[oldSession.ID] != "old-session" || listedIDs[newSession.ID] != "new-session" {
		t.Fatalf("aggregate list lost generation ownership: %#v", listedIDs)
	}
	counts := remote.ResourceCounts()
	if err := remote.LastError(); err != nil || counts["sessions"] != 2 || counts["processes"] != 2 || counts["attachments"] != 2 {
		t.Fatalf("aggregate resources: counts=%v err=%v", counts, err)
	}

	if _, err := remote.Create(context.Background(), session.CreateRequest{
		Name:    "old-session",
		Command: routerShellCommand(shell, newWorkspace, "exec sleep 60"),
	}); !errors.Is(err, session.ErrSessionExists) {
		t.Fatalf("cross-owner name collision: got %v", err)
	}
	if _, err := remote.Create(context.Background(), session.CreateRequest{
		Name:    "third-session",
		Command: routerShellCommand(shell, newWorkspace, "exec sleep 60"),
	}); !errors.Is(err, session.ErrResourceLimit) {
		t.Fatalf("global retained-session limit: got %v", err)
	}
	// A previous owner that cannot start leaves only its own sessions
	// unavailable; the active generation keeps serving terminals.
	delete(available, oldGeneration)
	if listed := remote.List(); remote.LastError() != nil || len(listed) != 1 || listed[0].ID != newSession.ID {
		t.Fatalf("unavailable old owner must not hide active sessions: sessions=%#v err=%v", listed, remote.LastError())
	}
	if _, err := remote.Snapshot(oldSession.ID); err == nil {
		t.Fatal("session of an unavailable owner was served")
	}
}

func TestRouterReopensArchivedSessionOnCurrentOwnerCodeWithoutCatalogMutation(t *testing.T) {
	oldManager, oldWorkspace, shell := newRouterTestManager(t)
	activeManager, activeWorkspace, _ := newRouterTestManager(t)
	oldGeneration, activeGeneration := generation(41), generation(42)
	staleDescriptor := testOwnerDescriptor(t, oldGeneration, oldWorkspace, shell)
	activeDescriptor := testOwnerDescriptor(t, activeGeneration, activeWorkspace, shell)
	activeDescriptor.Source.Version = "current"
	activeDescriptor.Source.SHA256 = strings.Repeat("b", 64)
	activeDescriptor.Source.Length = 2
	activeDescriptor.Executable = filepath.Join(t.TempDir(), "paperboat-current")
	if err := activeDescriptor.Validate(); err != nil {
		t.Fatalf("current active descriptor: %v", err)
	}

	oldSession, err := oldManager.Create(context.Background(), session.CreateRequest{
		Name:    "archived-shell",
		Command: routerShellCommand(shell, oldWorkspace, `printf 'ready'; IFS= read -r input; printf 'after-update:%s' "$input"; exec sleep 60`),
	})
	if err != nil {
		t.Fatalf("create archived session: %v", err)
	}

	// The protected catalog still held the old source when Connections built
	// its router. Reconcile then rewrites this stopped generation to the active
	// verified source; Ensure returns that freshly authenticated descriptor.
	reopenedDescriptor := staleDescriptor
	reopenedDescriptor.Source = activeDescriptor.Source
	reopenedDescriptor.Executable = activeDescriptor.Executable
	if err := reopenedDescriptor.Validate(); err != nil {
		t.Fatalf("reopened descriptor: %v", err)
	}
	reopenedOwner := authenticatedRouterOwner(t, oldGeneration, oldManager, oldWorkspace, shell)
	reopenedOwner.Descriptor = reopenedDescriptor
	reopenedOwner.Identity.ExecutableSHA256 = activeDescriptor.Source.SHA256
	activeOwner := authenticatedRouterOwner(t, activeGeneration, activeManager, activeWorkspace, shell)
	activeOwner.Descriptor = activeDescriptor
	activeOwner.Identity.ExecutableSHA256 = activeDescriptor.Source.SHA256
	metadata := map[string]Owner{
		oldGeneration:    {Descriptor: staleDescriptor},
		activeGeneration: {Descriptor: activeDescriptor},
	}
	available := map[string]Owner{oldGeneration: reopenedOwner, activeGeneration: activeOwner}
	router, err := NewRouter(activeGeneration, metadata, 4, ensureRouterOwner(available))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := remote.Snapshot(oldSession.ID)
	if err != nil || snapshot.ID != oldSession.ID || snapshot.Name != "archived-shell" {
		t.Fatalf("old session reopened through current authenticated code: snapshot=%#v err=%v", snapshot, err)
	}
	if err := remote.LastError(); err != nil {
		t.Fatalf("router rejected the source transition before reaching its owner: %v", err)
	}
	if _, err := remote.AttachLive(oldSession.ID, "att_reopened"); err != nil {
		t.Fatalf("attach through reopened owner: %v", err)
	}
	key := session.InputKey{ClientID: "client-reopened", AttachmentID: "att_reopened", Generation: oldSession.Generation, InputID: "input-after-update"}
	if decision, err := remote.Write(oldSession.ID, key, []byte("kept\n")); err != nil || decision.Status != session.InputAccepted {
		t.Fatalf("write after owner reopen: decision=%#v err=%v", decision, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output []byte
	for !bytes.Contains(output, []byte("after-update:kept")) {
		event, err := remote.WaitNext(ctx, oldSession.ID, "att_reopened")
		if err != nil {
			t.Fatalf("read input response after owner reopen: %q: %v", output, err)
		}
		output = append(output, event.Data...)
	}
	if len(activeManager.List()) != 0 || len(oldManager.List()) != 1 {
		t.Fatal("archived session moved away from its original generation store")
	}
}

func TestRouterRejectsArchivedCodeTransitionOutsideCurrentActiveSource(t *testing.T) {
	oldManager, oldWorkspace, shell := newRouterTestManager(t)
	activeManager, activeWorkspace, _ := newRouterTestManager(t)
	oldGeneration, activeGeneration := generation(43), generation(44)
	oldDescriptor := testOwnerDescriptor(t, oldGeneration, oldWorkspace, shell)
	activeDescriptor := testOwnerDescriptor(t, activeGeneration, activeWorkspace, shell)
	activeDescriptor.Source.Version = "current"
	activeDescriptor.Source.SHA256 = strings.Repeat("b", 64)
	activeDescriptor.Source.Length = 2
	activeDescriptor.Executable = filepath.Join(t.TempDir(), "paperboat-current")

	metadata := map[string]Owner{
		oldGeneration:    {Descriptor: oldDescriptor},
		activeGeneration: {Descriptor: activeDescriptor},
	}
	activeOwner := authenticatedRouterOwner(t, activeGeneration, activeManager, activeWorkspace, shell)
	activeOwner.Descriptor = activeDescriptor
	activeOwner.Identity.ExecutableSHA256 = activeDescriptor.Source.SHA256

	for name, alter := range map[string]func(*Descriptor){
		"unrecognized source": func(descriptor *Descriptor) {
			descriptor.Source = activeDescriptor.Source
			descriptor.Source.SHA256 = strings.Repeat("c", 64)
			descriptor.Source.Length = 3
			descriptor.Executable = activeDescriptor.Executable
		},
		"changed immutable workspace": func(descriptor *Descriptor) {
			descriptor.Source = activeDescriptor.Source
			descriptor.Executable = activeDescriptor.Executable
			descriptor.Workspace = filepath.Join(oldWorkspace, "other")
		},
	} {
		t.Run(name, func(t *testing.T) {
			descriptor := oldDescriptor
			alter(&descriptor)
			if err := descriptor.Validate(); err != nil {
				t.Fatalf("transition descriptor: %v", err)
			}
			owner := authenticatedRouterOwner(t, oldGeneration, oldManager, oldWorkspace, shell)
			owner.Descriptor = descriptor
			owner.Identity.ExecutableSHA256 = descriptor.Source.SHA256
			available := map[string]Owner{oldGeneration: owner, activeGeneration: activeOwner}
			router, err := NewRouter(activeGeneration, metadata, 4, ensureRouterOwner(available))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := router.Call(context.Background(), "List", []byte(`{}`)); !errors.Is(err, session.ErrOwnerUnavailable) {
				t.Fatalf("untrusted source transition was accepted: %v", err)
			}
		})
	}
}

func TestRouterBoundsCatalogAndFailsClosedOnOverLimitOwnerList(t *testing.T) {
	first, firstWorkspace, shell := newRouterTestManager(t)
	second, secondWorkspace, _ := newRouterTestManager(t)
	firstGeneration, secondGeneration, thirdGeneration := generation(11), generation(12), generation(13)
	firstOwner := authenticatedRouterOwner(t, firstGeneration, first, firstWorkspace, shell)
	secondOwner := authenticatedRouterOwner(t, secondGeneration, second, secondWorkspace, shell)
	available := map[string]Owner{firstGeneration: firstOwner, secondGeneration: secondOwner}
	metadata := map[string]Owner{
		firstGeneration:  {Descriptor: firstOwner.Descriptor},
		secondGeneration: {Descriptor: secondOwner.Descriptor},
	}
	router, err := NewRouter(firstGeneration, metadata, 1, ensureRouterOwner(available))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"List", "ResourceCounts"} {
		if _, err := router.Call(context.Background(), op, []byte(`{"unexpected":true}`)); !errors.Is(err, session.ErrInvalidSession) {
			t.Fatalf("malformed %s request must be rejected: %v", op, err)
		}
	}
	for index, name := range []string{"bound-one", "bound-two"} {
		if _, err := first.Create(context.Background(), session.CreateRequest{
			Name:    name,
			Command: routerShellCommand(shell, firstWorkspace, "exec sleep 60"),
		}); err != nil {
			t.Fatalf("seed owner session %d: %v", index, err)
		}
	}
	// Sessions an owner already holds past the ceiling stay listed and reachable; the ceiling only
	// refuses new sessions.
	if _, err := router.Call(context.Background(), "List", []byte(`{}`)); err != nil {
		t.Fatalf("list with sessions past the ceiling: %v", err)
	}
	if _, err := router.Call(context.Background(), "Create", []byte(`{"Request":{"Name":"past-ceiling"}}`)); !errors.Is(err, session.ErrResourceLimit) {
		t.Fatalf("create past the ceiling must be refused: got %v", err)
	}

	thirdOwner := Owner{Descriptor: testOwnerDescriptor(t, thirdGeneration, t.TempDir(), shell)}
	tooMany := map[string]Owner{
		firstGeneration:  metadata[firstGeneration],
		secondGeneration: metadata[secondGeneration],
		thirdGeneration:  thirdOwner,
	}
	if err := router.SetOwners(firstGeneration, tooMany); !errors.Is(err, session.ErrInvalidSession) {
		t.Fatalf("catalog owner bound: got %v", err)
	}
}

func TestRouterRejectsDuplicateSessionIDsAndMismatchedOwnerIdentity(t *testing.T) {
	first, firstWorkspace, shell := newRouterTestManager(t)
	second, secondWorkspace, _ := newRouterTestManager(t)
	firstGeneration, secondGeneration := generation(21), generation(22)
	firstOwner := authenticatedRouterOwner(t, firstGeneration, first, firstWorkspace, shell)
	secondOwner := authenticatedRouterOwner(t, secondGeneration, second, secondWorkspace, shell)
	sharedID := "terminal_00000000-0000-4000-8000-000000000021"
	for index, entry := range []struct {
		manager   *session.Manager
		workspace string
		name      string
	}{
		{first, firstWorkspace, "collision-one"},
		{second, secondWorkspace, "collision-two"},
	} {
		if _, err := entry.manager.Create(context.Background(), session.CreateRequest{
			ID:      sharedID,
			Name:    entry.name,
			Command: routerShellCommand(shell, entry.workspace, "exec sleep 60"),
		}); err != nil {
			t.Fatalf("seed duplicate ID owner %d: %v", index, err)
		}
	}
	metadata := map[string]Owner{
		firstGeneration:  {Descriptor: firstOwner.Descriptor},
		secondGeneration: {Descriptor: secondOwner.Descriptor},
	}
	available := map[string]Owner{firstGeneration: firstOwner, secondGeneration: secondOwner}
	router, err := NewRouter(firstGeneration, metadata, 2, ensureRouterOwner(available))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Call(context.Background(), "List", []byte(`{}`)); !errors.Is(err, session.ErrOwnerUnavailable) {
		t.Fatalf("duplicate session ID across stores must fail closed: %v", err)
	}

	mismatchedIdentity := secondOwner
	mismatchedIdentity.Identity.Generation = firstGeneration
	available[secondGeneration] = mismatchedIdentity
	identityRouter, err := NewRouter(firstGeneration, metadata, 2, ensureRouterOwner(available))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identityRouter.Call(context.Background(), "List", []byte(`{}`)); !errors.Is(err, session.ErrOwnerUnavailable) {
		t.Fatalf("mismatched authenticated generation must fail closed: %v", err)
	}

	retiredOwner := secondOwner
	retiredOwner.Identity.Retired = true
	available[secondGeneration] = retiredOwner
	retiredRouter, err := NewRouter(firstGeneration, metadata, 2, ensureRouterOwner(available))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retiredRouter.Call(context.Background(), "List", []byte(`{}`)); !errors.Is(err, session.ErrOwnerUnavailable) {
		t.Fatalf("retired owner must not receive session RPCs: %v", err)
	}

	otherAccount := secondOwner.Descriptor
	otherAccount.RuntimeRoot = filepath.Join(secondWorkspace, "other-runtime")
	if err := otherAccount.Validate(); err != nil {
		t.Fatalf("valid cross-account descriptor fixture: %v", err)
	}
	crossAccount := map[string]Owner{
		firstGeneration:  {Descriptor: firstOwner.Descriptor},
		secondGeneration: {Descriptor: otherAccount},
	}
	if _, err := NewRouter(firstGeneration, crossAccount, 2, ensureRouterOwner(available)); !errors.Is(err, session.ErrInvalidSession) {
		t.Fatalf("cross-account owner catalog must be rejected: %v", err)
	}
}

func TestRouterSerializesConcurrentCreateAtGlobalLimit(t *testing.T) {
	manager, workspace, shell := newRouterTestManager(t)
	gen := generation(31)
	owner := authenticatedRouterOwner(t, gen, manager, workspace, shell)
	metadata := map[string]Owner{gen: {Descriptor: owner.Descriptor}}
	router, err := NewRouter(gen, metadata, 1, ensureRouterOwner(map[string]Owner{gen: owner}))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		snapshot session.Snapshot
		err      error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, name := range []string{"concurrent-one", "concurrent-two"} {
		name := name
		wait.Add(1)
		go func() {
			defer wait.Done()
			snapshot, err := remote.Create(context.Background(), session.CreateRequest{
				Name:    name,
				Command: routerShellCommand(shell, workspace, "exec sleep 60"),
			})
			results <- result{snapshot: snapshot, err: err}
		}()
	}
	wait.Wait()
	close(results)
	successes, limited := 0, 0
	for result := range results {
		switch {
		case result.err == nil && result.snapshot.ID != "":
			successes++
		case errors.Is(result.err, session.ErrResourceLimit):
			limited++
		default:
			t.Fatalf("concurrent create result=%#v", result)
		}
	}
	if successes != 1 || limited != 1 || len(manager.List()) != 1 {
		t.Fatalf("global capacity overbooked: successes=%d limited=%d sessions=%#v", successes, limited, manager.List())
	}
}

func newRouterTestManager(t *testing.T) (*session.Manager, string, string) {
	t.Helper()
	workspace := t.TempDir()
	adapter, err := pty.NewAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{
		Launch:             func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) },
		MaxSessions:        8,
		MaxAttachments:     8,
		MaxInputDecisions:  64,
		HistoryBytes:       1 << 20,
		AttachmentBytes:    1 << 20,
		TerminationTimeout: 3 * time.Second,
		TerminationGrace:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	shell := "/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		shell = "/usr/bin/sh"
	}
	return manager, workspace, shell
}

func authenticatedRouterOwner(t *testing.T, gen string, manager session.Service, workspace, shell string) Owner {
	t.Helper()
	descriptor := testOwnerDescriptor(t, gen, workspace, shell)
	identity := workloadbridge.OwnerIdentity{Schema: Schema, Generation: gen, ExecutableSHA256: descriptor.Source.SHA256}
	return Owner{
		Descriptor: descriptor,
		Identity:   identity,
		Call: func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
			return session.ServeRemoteCall(ctx, manager, op, body)
		},
	}
}

func testOwnerDescriptor(t *testing.T, gen, workspace, shell string) Descriptor {
	t.Helper()
	source := installsource.Source{
		Version:      "test",
		Platform:     runtime.GOOS,
		Architecture: runtime.GOARCH,
		SHA256:       strings.Repeat("a", 64),
		Length:       1,
		Distribution: installsource.Custom,
	}
	descriptor := Descriptor{
		Schema:      Schema,
		Generation:  gen,
		Instance:    "router-test-account",
		Source:      source,
		Executable:  shell,
		RuntimeRoot: filepath.Join(os.TempDir(), "paperboat-router-test-runtime"),
		Endpoint:    filepath.Join("/tmp", "pb-"+gen[:8]+".sock"),
		UID:         os.Geteuid(),
		GID:         os.Getegid(),
		Workspace:   workspace,
		Shell:       shell,
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("test descriptor: %v", err)
	}
	return descriptor
}

func ensureRouterOwner(owners map[string]Owner) Ensure {
	return func(_ context.Context, gen string) (Owner, error) {
		owner, ok := owners[gen]
		if !ok {
			return Owner{}, session.ErrOwnerUnavailable
		}
		return owner, nil
	}
}

func generation(value uint64) string { return fmt.Sprintf("%032x", value) }

func routerShellCommand(shell, workspace, script string) pty.Command {
	return pty.Command{
		Path:       shell,
		Args:       []string{"-c", script},
		Env:        []string{"PATH=/usr/bin:/bin", "TERM=xterm"},
		CWD:        workspace,
		Dimensions: pty.Dimensions{Columns: 80, Rows: 24},
	}
}

// F3-05 on hp: a host whose owner kept more closed records than the session limit refused every new
// terminal with resource_limit while no session was live. Closed records must not consume the limit.
func TestRouterAdmitsNewSessionsWhileClosedRecordsExceedTheLimit(t *testing.T) {
	manager, workspace, shell := newRouterTestManager(t)
	gen := generation(41)
	owner := authenticatedRouterOwner(t, gen, manager, workspace, shell)
	router, err := NewRouter(gen, map[string]Owner{gen: {Descriptor: owner.Descriptor}}, 1, ensureRouterOwner(map[string]Owner{gen: owner}))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"closed-one", "closed-two", "live-three"} {
		created, err := remote.Create(context.Background(), session.CreateRequest{Name: name, Command: routerShellCommand(shell, workspace, "exec sleep 60")})
		if err != nil {
			t.Fatalf("create %s after closing the earlier ones: %v", name, err)
		}
		if name == "live-three" {
			break
		}
		if _, err := manager.Close(context.Background(), created.ID); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
	}
	if _, err := remote.Create(context.Background(), session.CreateRequest{Name: "over-limit", Command: routerShellCommand(shell, workspace, "exec sleep 60")}); !errors.Is(err, session.ErrResourceLimit) {
		t.Fatalf("a second live session must still be refused: %v", err)
	}
}

// F3-05 on three hosts: the 20th terminal was created but attaching to it was refused with
// resource_limit. A machine at its terminal ceiling attaches to every one of its sessions.
func TestRouterAttachesToEverySessionAtTheCeiling(t *testing.T) {
	const ceiling = 3
	manager, workspace, shell := newRouterTestManager(t)
	gen := generation(51)
	owner := authenticatedRouterOwner(t, gen, manager, workspace, shell)
	router, err := NewRouter(gen, map[string]Owner{gen: {Descriptor: owner.Descriptor}}, ceiling, ensureRouterOwner(map[string]Owner{gen: owner}))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < ceiling; i++ {
		created, err := remote.Create(context.Background(), session.CreateRequest{Name: fmt.Sprintf("ceiling-%d", i), Command: routerShellCommand(shell, workspace, "exec sleep 60")})
		if err != nil {
			t.Fatalf("create %d of %d: %v", i+1, ceiling, err)
		}
		ids = append(ids, created.ID)
	}
	for i, id := range ids {
		if _, err := remote.AttachLive(id, fmt.Sprintf("att_ceiling_%d", i)); err != nil {
			t.Fatalf("attach to session %d of %d at the ceiling: %v", i+1, ceiling, err)
		}
	}
	if _, err := remote.Create(context.Background(), session.CreateRequest{Name: "over-ceiling", Command: routerShellCommand(shell, workspace, "exec sleep 60")}); !errors.Is(err, session.ErrResourceLimit) {
		t.Fatalf("create past the ceiling: %v", err)
	}
	if got := len(manager.List()); got != ceiling {
		t.Fatalf("a refused create left %d sessions, want %d", got, ceiling)
	}
}
