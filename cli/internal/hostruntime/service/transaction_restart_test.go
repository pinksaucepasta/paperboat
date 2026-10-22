package service

import (
	"bytes"
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// F14-03: a failed replacement restores the previous executable, but a role that was (re)started
// from the candidate's executable keeps running the candidate's image until it is restarted, so an
// updater or supervisor from the wrong version outlives the rollback. Roles that run the replaced
// executable are restarted on rollback; a role on a pinned executable (the carrier) is left alone.
func TestLifecycleRollbackRestartsRolesThatRunTheReplacedExecutable(t *testing.T) {
	root := shorttemp.Dir(t)
	image := filepath.Join(root, "image")
	worker := "/opt/paperboat/bin/pb"
	swapped := true
	var calls []string
	var mu sync.Mutex
	newRole := func(id, executable string) *fakeTransactionalComponent {
		component := newFakeTransactionalComponent(id, &calls, &mu)
		component.status = NativeComponentStatus{ID: id, Installed: true, Enabled: true, Running: true, Ready: true, Definition: []byte("ExecStart=" + executable)}
		return component
	}
	carrier := newRole("carrier", "/opt/paperboat/releases/pins/abc/pb")
	hostd := newRole("hostd", worker)
	updater := newRole("updater", worker)
	stopped := newRole("idle-role", worker)
	stopped.status.Running, stopped.status.Ready = false, false
	if err := writeFile(image, "prior exact image and configuration"); err != nil {
		t.Fatal(err)
	}
	manager, err := NewLifecycleManager(LifecycleConfig{
		StateRoot: filepath.Join(root, "lifecycle"), NativeInstallation: fileInstallationState{image},
		Components:        []TransactionalComponent{carrier, hostd, updater, stopped},
		RunsReplacedImage: func(status NativeComponentStatus) bool { return bytes.Contains(status.Definition, []byte(worker)) },
		ImageSwapped:      func([]byte) bool { return swapped },
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("the new version's daemon never became ready")
	err = manager.Replace(context.Background(), func(context.Context) error {
		mu.Lock()
		calls = nil // only the rollback is under test
		mu.Unlock()
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("replace error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, ",")
	for _, want := range []string{"hostd:stop", "hostd:restore", "updater:stop", "updater:restore"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rollback calls %q lack %q", joined, want)
		}
	}
	for _, unwanted := range []string{"carrier:", "idle-role:"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("rollback touched %q: %q", unwanted, joined)
		}
	}
	if status, _ := updater.Inspect(context.Background()); !status.Running || !status.Ready {
		t.Fatalf("restarted updater is not running: %+v", status)
	}
}

func writeFile(path, content string) error {
	return fileInstallationState{path}.Restore(context.Background(), []byte(content))
}

// A replacement that failed before the executable was swapped restarts nothing: the roles still run
// the image they started with.
func TestLifecycleRollbackLeavesRolesAloneWhenTheExecutableWasNeverSwapped(t *testing.T) {
	root := shorttemp.Dir(t)
	image := filepath.Join(root, "image")
	if err := writeFile(image, "prior exact image and configuration"); err != nil {
		t.Fatal(err)
	}
	var calls []string
	var mu sync.Mutex
	hostd := newFakeTransactionalComponent("hostd", &calls, &mu)
	hostd.status = NativeComponentStatus{ID: "hostd", Installed: true, Enabled: true, Running: true, Ready: true, Definition: []byte("ExecStart=/opt/paperboat/bin/pb")}
	manager, err := NewLifecycleManager(LifecycleConfig{
		StateRoot: filepath.Join(root, "lifecycle"), NativeInstallation: fileInstallationState{image},
		Components:        []TransactionalComponent{hostd},
		RunsReplacedImage: func(NativeComponentStatus) bool { return true },
		ImageSwapped:      func([]byte) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("no space left on device")
	if err := manager.Replace(context.Background(), func(context.Context) error { mu.Lock(); calls = nil; mu.Unlock(); return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("a failure before the executable was swapped disturbed the roles: %v", calls)
	}
}
