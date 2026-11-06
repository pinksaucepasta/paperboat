package service

import (
	"bytes"
	"context"
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

	restartSelf, err := manager.RecoverWithin(context.Background(), "updater")
	if err != nil || !restartSelf {
		t.Fatalf("restartSelf=%v err=%v", restartSelf, err)
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
	restartSelf, err = manager.RecoverWithin(context.Background(), "updater")
	if err != nil || restartSelf {
		t.Fatalf("a finished recovery ran again: restartSelf=%v err=%v", restartSelf, err)
	}
}
