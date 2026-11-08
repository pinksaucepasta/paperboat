package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// The updater recovers an interrupted install from inside its own service. Its rollback stops every role that
// runs the replaced executable, and the updater runs it: stopping itself would end the recovery half done. The
// caller's own role is left running and reported, so it can exit and be started on the restored executable.
func TestRecoverWithinNeverStopsTheCallersOwnService(t *testing.T) {
	root := shorttemp.Dir(t)
	image := filepath.Join(root, "image")
	if err := writeFile(image, "prior exact image and configuration"); err != nil {
		t.Fatal(err)
	}
	worker := "/opt/paperboat/bin/pb"
	var calls []string
	var mu sync.Mutex
	role := func(id string) *fakeTransactionalComponent {
		component := newFakeTransactionalComponent(id, &calls, &mu)
		component.status = NativeComponentStatus{ID: id, Installed: true, Enabled: true, Running: true, Ready: true, Definition: []byte("ExecStart=" + worker)}
		return component
	}
	hostd, updater := role("hostd"), role("updater")
	config := LifecycleConfig{
		StateRoot: filepath.Join(root, "lifecycle"), NativeInstallation: fileInstallationState{image},
		Components:        []TransactionalComponent{hostd, updater},
		RunsReplacedImage: func(status NativeComponentStatus) bool { return bytes.Contains(status.Definition, []byte(worker)) },
		ImageSwapped:      func([]byte) bool { return true }, // killed after binaries_activated: the candidate is in place
	}
	manager, err := NewLifecycleManager(config)
	if err != nil {
		t.Fatal(err)
	}
	// The install dies inside the replacement: its journal stays, nothing rolls back.
	func() {
		defer func() { _ = recover() }()
		_ = manager.Replace(context.Background(), func(context.Context) error {
			updater.mu.Lock() // the candidate's unit definition was already written
			updater.status.Definition = []byte("ExecStart=" + worker + " --candidate")
			updater.mu.Unlock()
			panic("killed")
		})
	}()
	mu.Lock()
	calls = nil
	mu.Unlock()

	finishSelf, err := manager.RecoverWithin(context.Background(), "updater")
	if err != nil || finishSelf == nil {
		t.Fatalf("finishSelf=%v err=%v", finishSelf != nil, err)
	}
	mu.Lock()
	joined := strings.Join(calls, ",")
	mu.Unlock()
	if strings.Contains(joined, "updater:stop") {
		t.Fatalf("the recovery stopped the service it runs in: %s", joined)
	}
	for _, want := range []string{"hostd:stop", "hostd:restore", "updater:restore"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("recovery calls %q lack %q", joined, want)
		}
	}
	// The recovery is done once: the next start finds nothing.
	finishSelf, err = manager.RecoverWithin(context.Background(), "updater")
	if err != nil || finishSelf != nil {
		t.Fatalf("a finished recovery ran again: finishSelf=%v err=%v", finishSelf != nil, err)
	}
}

type killedOnInstall struct{ *fakeTransactionalComponent }

func (k killedOnInstall) Install(context.Context) error { panic("killed") }

// An install on a machine that had no updater installs the updater, then dies. The updater recovers it from inside
// its own service, whose pre-install state is "not installed": removing it in the rollback loop would end the
// recovery with the other roles unrestored and the journal left behind. Every other role is restored and the
// journal removed first; the updater's own removal is handed back as the caller's last step.
func TestRecoverWithinRemovesTheCallersOwnServiceLast(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	hostd := newFakeTransactionalComponent("hostd", &calls, &mu)
	updater := newFakeTransactionalComponent("updater", &calls, &mu)
	host := killedOnInstall{newFakeTransactionalComponent("host", &calls, &mu)}
	manager := lifecycleForTest(t, hostd, updater, host)
	func() {
		defer func() { _ = recover() }()
		_ = manager.Install(context.Background())
	}()
	mu.Lock()
	calls = nil
	mu.Unlock()

	finishSelf, err := manager.RecoverWithin(context.Background(), "updater")
	if err != nil || finishSelf == nil {
		t.Fatalf("finishSelf=%v err=%v", finishSelf != nil, err)
	}
	mu.Lock()
	joined := strings.Join(calls, ",")
	mu.Unlock()
	if joined != "hostd:restore" {
		t.Fatalf("recovery calls %q, want only hostd restored before the caller's last step", joined)
	}
	if _, err := os.Lstat(manager.journal); !os.IsNotExist(err) {
		t.Fatalf("the journal must be gone before the caller removes its own service: %v", err)
	}
	if err := finishSelf(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, _ := updater.Inspect(context.Background()); status.Installed || status.Running {
		t.Fatalf("the updater was not put back to not installed: %+v", status)
	}
}

// The activation helper is another unit, outside every component: its recovery (self "") restores every role, the
// updater's own removal included, inside the rollback, and leaves nothing for last. Skipping recovery in the helper
// left the install journal behind, and the helper failed on it until the updater service took the lock.
func TestRecoverWithinNoSelfRestoresEveryRole(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	hostd := newFakeTransactionalComponent("hostd", &calls, &mu)
	updater := newFakeTransactionalComponent("updater", &calls, &mu)
	host := killedOnInstall{newFakeTransactionalComponent("host", &calls, &mu)}
	manager := lifecycleForTest(t, hostd, updater, host)
	func() {
		defer func() { _ = recover() }()
		_ = manager.Install(context.Background())
	}()
	finishSelf, err := manager.RecoverWithin(context.Background(), "")
	if err != nil || finishSelf != nil {
		t.Fatalf("finishSelf=%v err=%v", finishSelf != nil, err)
	}
	for _, component := range []*fakeTransactionalComponent{hostd, updater} {
		if status, _ := component.Inspect(context.Background()); status.Installed {
			t.Fatalf("%s was not restored to not installed: %+v", component.id, status)
		}
	}
	if _, err := os.Lstat(manager.journal); !os.IsNotExist(err) {
		t.Fatalf("journal left behind: %v", err)
	}
}
