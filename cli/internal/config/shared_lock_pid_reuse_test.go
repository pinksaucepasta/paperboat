//go:build linux || darwin || windows

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F21-159: the PID in a lock's owner record may now belong to a different process. A live process that started
// before the lock was created is the owner (not stale); one that started after the lock was created is a
// recycled PID (stale), which kill(pid, 0) alone cannot tell apart.
func TestSharedLockIsNotTrustedAcrossAReusedPID(t *testing.T) {
	hostname, _ := os.Hostname()
	check := func(created time.Time) bool {
		t.Helper()
		path := filepath.Join(t.TempDir(), "lock.d")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		owner, _ := json.Marshal(sharedLockOwner{PID: os.Getpid(), Hostname: hostname, CreatedAt: created, Token: "t"})
		if err := os.WriteFile(filepath.Join(path, "owner.json"), owner, 0o600); err != nil {
			t.Fatal(err)
		}
		stale, err := sharedLockIsStale(path, hostname)
		if err != nil {
			t.Fatal(err)
		}
		return stale
	}
	if check(time.Now()) {
		t.Fatal("a lock created by this live process was called stale")
	}
	if !check(time.Now().Add(-24 * time.Hour)) {
		t.Fatal("a lock created before this process started was trusted: the PID was reused")
	}
}
