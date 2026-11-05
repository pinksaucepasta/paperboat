//go:build linux

package sessionowner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// dadape: the updater's reconcile pass hit its 2-minute deadline on every
// update. Each old owner that did not answer retirement held the pass for
// the bridge's 30 s call timeout. An owner that never confirms retirement is
// now bounded, named in the error, and left running for the next pass.
func TestReconcileBoundsAnOwnerThatWillNotRetire(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned catalog fixture")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-stuck-")
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
	runner := &runningOwnerRunner{}
	catalog := Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "root", Group: "root", Maximum: 4, Runner: runner}
	old := Descriptor{Schema: Schema, Generation: generation(1), Instance: "stuck-test", Source: source, Executable: pin, RuntimeRoot: runtimeRoot, InitialStore: true, Endpoint: filepath.Join(root, "old-run", "control.sock"), UID: 0, GID: os.Getgid(), Workspace: root, Shell: "/bin/sh"}
	active := old
	active.Generation, active.InitialStore, active.Endpoint = generation(2), false, filepath.Join(root, "active.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, d := range []Descriptor{old, active} {
		if err = catalog.Publish(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, catalog, old)
	if err = os.Mkdir(filepath.Dir(old.Endpoint), 0700); err != nil {
		t.Fatal(err)
	}
	token, err := ReadCapability(old)
	if err != nil {
		t.Fatal(err)
	}
	manager, _, _ := newRouterTestManager(t)
	stuck := make(chan struct{})
	server := &workloadbridge.Server{Endpoint: old.Endpoint, Token: token, Sessions: manager, SessionOnly: true,
		Identity:          &workloadbridge.OwnerIdentity{Schema: Schema, Generation: old.Generation, ExecutableSHA256: source.SHA256},
		MaximumConcurrent: 8, Ready: make(chan error, 1),
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: old.Generation, Epoch: 1}
		},
		AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil },
		Retire: func(ctx context.Context) error {
			select {
			case <-stuck:
			case <-ctx.Done():
			}
			return ctx.Err()
		}}
	serverCtx, stopServer := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Run(serverCtx) }()
	if err = <-server.Ready; err != nil {
		t.Fatal(err)
	}
	defer func() { close(stuck); stopServer(); <-done }()

	started := time.Now()
	err = Coordinator{Catalog: catalog, Active: active}.Reconcile(ctx)
	elapsed := time.Since(started)
	if elapsed > ownerRetireTimeout+3*time.Second {
		t.Fatalf("reconcile waited %s for an owner that will not retire", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), old.Generation) {
		t.Fatalf("reconcile did not name the owner it waited for: %v", err)
	}
	if runner.removals != 0 {
		t.Fatal("an owner that never confirmed retirement was removed")
	}
	if _, err := Find(catalog.Root, old.Generation); err != nil {
		t.Fatalf("unconfirmed owner left the catalog: %v", err)
	}
}

// runningOwnerRunner reports every owner unit as loaded and running.
type runningOwnerRunner struct{ removals int }

func (r *runningOwnerRunner) Run(_ context.Context, name string, args ...string) error {
	if name != "systemctl" || len(args) == 0 {
		return errors.New("unexpected native mutation")
	}
	if args[0] == "disable" || args[0] == "stop" {
		r.removals++
	}
	return nil
}

func (r *runningOwnerRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	if name != "systemctl" || len(args) == 0 || args[0] != "show" {
		return "", errors.New("unexpected native probe")
	}
	return "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\n", nil
}
