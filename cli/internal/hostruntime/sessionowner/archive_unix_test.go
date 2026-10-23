//go:build linux

package sessionowner

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the actual authenticated reader and router: stopped history must
// participate in global name/ID admission without reopening its native owner.
func TestArchivedHistoryAdmitsNewTerminalWithoutNativeStartup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned catalog fixture")
	}
	root, e := os.MkdirTemp("/var/lib", "pb-archive-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(root)
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	image, e := os.ReadFile(self)
	if e != nil {
		t.Fatal(e)
	}
	pin := filepath.Join(root, "pb")
	if e = os.WriteFile(pin, image, 0755); e != nil {
		t.Fatal(e)
	}
	source, e := installsource.Inspect(pin, "qualification", installsource.Custom)
	if e != nil {
		t.Fatal(e)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if e = os.Mkdir(runtimeRoot, 0700); e != nil {
		t.Fatal(e)
	}
	catalog := Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4}
	runner := &absentOwnerRunner{}
	catalog.Runner = runner
	oldRun := filepath.Join(root, "old-run")
	if e = os.Mkdir(oldRun, 0700); e != nil {
		t.Fatal(e)
	}
	old := Descriptor{Schema: Schema, Generation: generation(1), Instance: "archive-test", Source: source, Executable: pin, RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(oldRun, "control.sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	active := old
	active.Generation = generation(2)
	active.InitialStore = false
	active.Endpoint = filepath.Join(root, "active.sock")
	active.Source.Version = "current-qualification"
	active.Executable = filepath.Join(root, "current-pb")
	if e = os.WriteFile(active.Executable, image, 0755); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, d := range []Descriptor{old, active} {
		if e = catalog.Publish(ctx, d); e != nil {
			t.Fatal(e)
		}
	}
	durable, e := store.Open(ctx, store.Config{Root: old.StoreRoot()})
	if e != nil {
		t.Fatal(e)
	}
	record := store.Session{ID: "closed-history", Name: "reserved-name", CWD: root, Columns: 80, Rows: 24, State: "closed", Generation: 1}
	if e = durable.CreateSession(ctx, record); e != nil {
		t.Fatal(e)
	}
	if e = durable.Close(); e != nil {
		t.Fatal(e)
	}
	reader, e := NewArchiveReader(catalog.Root, active, 4)
	if e != nil {
		t.Fatal(e)
	}
	manager, workspace, shell := newRouterTestManager(t)
	token, e := ReadCapability(active)
	if e != nil {
		t.Fatal(e)
	}
	defer clear(token)
	server := &workloadbridge.Server{Endpoint: active.Endpoint, Token: token, Sessions: manager, SessionOnly: true, ArchiveMetadata: reader, Identity: &workloadbridge.OwnerIdentity{Schema: Schema, Generation: active.Generation, ExecutableSHA256: source.SHA256}, MaximumConcurrent: 8, Ready: make(chan error, 1), Fence: func() hostdproto.Status {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: active.Generation, Epoch: 1}
	}, AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil }}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if e = <-server.Ready; e != nil {
		t.Fatal(e)
	}
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	// Native uninstall already succeeded, but its cleanup/catalogue commit
	// was interrupted. Unknown IPC content preserves the old descriptor.
	coordinator := Coordinator{Catalog: catalog, Active: active}
	unknown := filepath.Join(oldRun, "unknown")
	if e = os.WriteFile(unknown, []byte("task-owned unexpected object"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = coordinator.Reconcile(ctx); e == nil {
		t.Fatal("unknown retirement content accepted")
	}
	oldPath, _ := Find(catalog.Root, old.Generation)
	if got, err := Read(oldPath); err != nil || got != old {
		t.Fatal("failed cleanup advanced catalogue")
	}
	if e = os.Remove(unknown); e != nil {
		t.Fatal(e)
	}
	installer, err := catalog.Installer(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := installer.PublishDefinition(ctx); err != nil {
		t.Fatal(err)
	}
	declaration := installer.DefinitionPath()
	originalDeclaration, err := os.ReadFile(declaration)
	if err != nil {
		t.Fatal(err)
	}
	unlockBusy, err := LockStore(old)
	if err != nil {
		t.Fatal(err)
	}
	if e = coordinator.Reconcile(ctx); !errors.Is(e, ErrBusy) {
		t.Fatalf("locked archive retirement: %v", e)
	}
	if got, err := Read(oldPath); err != nil || got != old {
		t.Fatal("busy archive advanced catalogue")
	}
	if _, err := os.Stat(oldRun); err != nil {
		t.Fatal("locked orphan archive lost IPC before zero-session proof")
	}
	if _, err := os.Stat(declaration); err != nil {
		t.Fatal("locked archive lost native declaration")
	}
	if e = unlockBusy(); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"changed", "symlink", "hardlink", "foreign"} {
		if err := os.Remove(declaration); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "preserved-declaration")
		if err := os.WriteFile(target, originalDeclaration, 0644); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "changed":
			err = os.WriteFile(declaration, []byte("unknown native declaration"), 0644)
		case "symlink":
			err = os.Symlink(target, declaration)
		case "hardlink":
			err = os.Link(target, declaration)
		case "foreign":
			err = os.WriteFile(declaration, originalDeclaration, 0644)
			if err == nil {
				err = os.Chown(declaration, 1, 1)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := coordinator.Reconcile(ctx); err == nil {
			t.Fatalf("%s native declaration accepted", kind)
		}
		if _, err := os.Lstat(declaration); err != nil {
			t.Fatal("unsafe native declaration removed")
		}
		if got, err := Read(oldPath); err != nil || got != old {
			t.Fatal("unsafe declaration advanced catalogue")
		}
		if body, err := os.ReadFile(target); err != nil || string(body) != string(originalDeclaration) {
			t.Fatal("foreign declaration target changed")
		}
		if err := os.Remove(declaration); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(declaration, originalDeclaration, 0644); err != nil {
			t.Fatal(err)
		}
	}
	runner.failRemoval = true
	if err := coordinator.Reconcile(ctx); err == nil {
		t.Fatal("native removal failure accepted")
	}
	if _, err := os.Stat(oldRun); err != nil {
		t.Fatal("failed native removal advanced IPC cleanup")
	}
	if got, err := Read(oldPath); err != nil || got != old {
		t.Fatal("failed native removal advanced catalogue")
	}
	runner.failRemoval = false
	if e = coordinator.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	reauthorized := old
	reauthorized.Source, reauthorized.Executable = active.Source, active.Executable
	if got, err := Read(oldPath); err != nil || got != reauthorized {
		t.Fatal("absent native owner retained stale executable authority")
	}
	old = reauthorized
	if e = coordinator.Reconcile(ctx); e != nil {
		t.Fatalf("retirement retry: %v", e)
	}
	empty := old
	empty.Generation, empty.InitialStore = generation(4), false
	empty.Endpoint = filepath.Join(root, "empty-run", "control.sock")
	if e = os.Mkdir(filepath.Dir(empty.Endpoint), 0700); e != nil {
		t.Fatal(e)
	}
	if e = catalog.Publish(ctx, empty); e != nil {
		t.Fatal(e)
	}
	emptyStore, err := store.Open(ctx, store.Config{Root: empty.StoreRoot()})
	if err != nil {
		t.Fatal(err)
	}
	if e = emptyStore.Close(); e != nil {
		t.Fatal(e)
	}
	if e = coordinator.Reconcile(ctx); e != nil {
		t.Fatal(e)
	}
	emptyPath, _ := Find(catalog.Root, empty.Generation)
	if _, err := os.Stat(emptyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty retired owner still consumes catalogue capacity")
	}
	if runner.starts != 0 || runner.removals != 2 {
		t.Fatalf("native retirement starts=%d removals=%d", runner.starts, runner.removals)
	}
	if _, err := os.Stat(declaration); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("leftover native declaration remains")
	}
	connections, e := NewConnections(ctx, catalog, active, func(context.Context, string) error { return errors.New("unexpected archived native startup") }, 4)
	if e != nil {
		t.Fatal(e)
	}
	defer connections.Close()
	remote, e := connections.Service()
	if e != nil {
		t.Fatal(e)
	}
	// Both cold and indexed inventory reads must fit their request budget
	// without starting an archived native owner.
	for _, indexed := range []bool{false, true} {
		observed, err := remote.Snapshot(record.ID)
		if err != nil || observed.ID != record.ID || observed.State != session.Closed {
			t.Fatalf("archived snapshot indexed=%t: state=%s error=%v", indexed, observed.State, err)
		}
	}
	if _, err := remote.SnapshotAtGeneration(record.ID, record.Generation+1); !errors.Is(err, session.ErrStaleGeneration) {
		t.Fatalf("archived snapshot lost generation fencing: %v", err)
	}
	snapshots := remote.List()
	if len(snapshots) != 1 || snapshots[0].ID != record.ID || snapshots[0].State != session.Closed {
		t.Fatalf("closed history unavailable: count=%d error=%v", len(snapshots), remote.LastError())
	}
	if _, e = remote.Create(ctx, session.CreateRequest{ID: "collision", Name: record.Name, Command: routerShellCommand(shell, workspace, "exec sleep 60")}); !errors.Is(e, session.ErrSessionExists) {
		t.Fatalf("history name admission: %v", e)
	}
	created, e := remote.Create(ctx, session.CreateRequest{ID: "new-terminal", Name: "fresh-name", Command: routerShellCommand(shell, workspace, "exec sleep 60")})
	if e != nil {
		t.Fatal(e)
	}
	if created.State != session.Running {
		t.Fatalf("new terminal state=%s", created.State)
	}
	if _, e = remote.Close(ctx, created.ID); e != nil {
		t.Fatal(e)
	}
	unlock, e := LockStore(old)
	if e != nil {
		t.Fatal(e)
	}
	metadata, e := reader(ctx, old.Generation)
	if e != nil || metadata.Available {
		t.Fatalf("live owner metadata admitted: %v", e)
	}
	if e = unlock(); e != nil {
		t.Fatal(e)
	}
	foreign := old
	foreign.Generation = generation(3)
	foreign.UID = 1
	foreign.RuntimeRoot = filepath.Join(root, "foreign-runtime")
	if e = os.Mkdir(foreign.RuntimeRoot, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Chown(foreign.RuntimeRoot, foreign.UID, foreign.GID); e != nil {
		t.Fatal(e)
	}
	foreign.InitialStore = false
	foreign.Endpoint = filepath.Join(root, "foreign.sock")
	if e = catalog.Publish(ctx, foreign); e != nil {
		t.Fatal(e)
	}
	if _, e = reader(ctx, foreign.Generation); !errors.Is(e, ErrInvalid) {
		t.Fatalf("foreign account read accepted: %v", e)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, e = reader(canceled, old.Generation); !errors.Is(e, context.Canceled) {
		t.Fatalf("canceled archive read: %v", e)
	}
}

type absentOwnerRunner struct {
	starts, removals int
	failRemoval      bool
}

func (r *absentOwnerRunner) Run(_ context.Context, name string, args ...string) error {
	if name != "systemctl" || len(args) == 0 {
		return errors.New("unexpected native mutation")
	}
	switch args[0] {
	case "disable":
		r.removals++
		if r.failRemoval {
			return errors.New("native removal failed")
		}
		return nil
	case "daemon-reload", "reset-failed":
		return nil
	default:
		r.starts++
		return errors.New("unexpected native startup")
	}
}
func (r *absentOwnerRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	if name != "systemctl" || len(args) == 0 || args[0] != "show" {
		return "", errors.New("unexpected native probe")
	}
	if strings.Contains(strings.Join(args, " "), "start") {
		return "", errors.New("unexpected native start")
	}
	return "LoadState=not-found\n", nil
}
