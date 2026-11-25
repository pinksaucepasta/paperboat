//go:build !windows

package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestLockOwner(t *testing.T, directory string, pid int) {
	t.Helper()
	if err := createSharedLockDirectory(directory); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	owner, _ := json.Marshal(sharedLockOwner{PID: pid, Hostname: hostname, CreatedAt: time.Now(), Token: "other"})
	if err := writeSharedLockOwner(filepath.Join(directory, "owner.json"), owner); err != nil {
		t.Fatal(err)
	}
}

// A pb killed in a container without init stays a zombie: kill(pid, 0) succeeds, but it releases nothing.
func TestSharedLockReclaimsAnUnreapedOwner(t *testing.T) {
	old := sharedLockWait
	t.Cleanup(func() { sharedLockWait = old })
	sharedLockWait = 5 * time.Second
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Wait() })
	lock := newSharedLock(filepath.Join(t.TempDir(), "profile.json.lock"))
	writeTestLockOwner(t, lock.path, child.Process.Pid)
	sharedLockWait = 200 * time.Millisecond
	err := lock.Lock()
	var timeout *CredentialLockTimeout
	if !errors.As(err, &timeout) || !errors.Is(err, errSharedLockTimeout) || timeout.PID != child.Process.Pid ||
		!strings.Contains(err.Error(), "held by pid") {
		t.Errorf("live owner: %v, want a typed timeout naming it", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	sharedLockWait = 5 * time.Second
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock of an exited, unreaped owner was not reclaimed: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

// A creator whose directory was replaced must not publish the new creator's temporary record as its own lock.
func TestSharedLockCreatorsDoNotShareATempOwner(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "profile.json.lock.d")
	if err := createSharedLockDirectory(directory); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(directory, "owner.json.tmp")
	if err := os.WriteFile(other, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSharedLockOwner(filepath.Join(directory, "owner.json"), []byte("mine")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "other" {
		t.Fatalf("another creator's record was touched: %q %v", b, err)
	}
}
