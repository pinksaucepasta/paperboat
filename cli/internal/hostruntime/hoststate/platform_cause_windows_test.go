//go:build windows

package hoststate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsHostStateCommitLockAndFreshReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "host-state")
	store, _, err := Open(Config{Root: root, Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal("protected store initialization failed")
	}
	defer store.Close()
	if _, err := acquireProcessLock(filepath.Join(root, lockFile)); !errors.Is(err, ErrLocked) || !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatal("actual lock contention lost native cause")
	}
	if revision, err := store.Commit(1, validState(t, 2, 1)); err != nil || revision != 2 {
		t.Fatal("protected store commit failed")
	}
	if err := store.Close(); err != nil {
		t.Fatal("owned store close failed")
	}
	reopened, status, err := Open(Config{Root: root, Clock: func() time.Time { return testNow.Add(time.Minute) }})
	if err != nil || status.Degraded {
		t.Fatal("fresh protected store reopen failed")
	}
	defer reopened.Close()
	state, revision, err := reopened.Snapshot()
	if err != nil || revision != 2 || len(state.Tunnels) != 1 || state.Tunnels[0].LastKnownGood == nil || state.Tunnels[0].LastKnownGood.Generation != 1 {
		t.Fatal("known generation or LKG lost")
	}
}

func TestWindowsHostStateNativeCauseBoundAndRecovery(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "protected-state")
	if _, err := readPrivateFile(path, 16); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatal("missing file native cause lost")
	}
	if err := protectWindowsObject(path, false); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatal("missing security object native cause lost")
	}
	if err := os.WriteFile(path, []byte("bounded-owned-fixture"), 0600); err != nil {
		t.Fatal("owned file creation failed")
	}
	if body, err := readPrivateFile(path, 8); body != nil || !errors.Is(err, ErrInvalidState) {
		clear(body)
		t.Fatal("oversized file bypassed bound")
	}
	if err := os.WriteFile(path, []byte("small"), 0600); err != nil {
		t.Fatal("owned file replacement failed")
	}
	body, err := readPrivateFile(path, 16)
	if err != nil || !bytes.Equal(body, []byte("small")) {
		clear(body)
		t.Fatal("bounded protected read did not recover")
	}
	clear(body)
	file, err := os.OpenFile(filepath.Join(root, "closed-lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal("closed object fixture creation failed")
	}
	if err := file.Close(); err != nil {
		t.Fatal("owned object close failed")
	}
	lock := &processLock{file: file}
	err = lock.Close()
	if !errors.Is(err, windows.ERROR_INVALID_HANDLE) || !errors.Is(err, os.ErrClosed) {
		t.Fatal("actual closed-handle unlock or close cause lost")
	}
	if lock.Close() != nil {
		t.Fatal("released owner did not become idempotent")
	}
}

func TestWindowsHostStateInvalidBackupFailsClosedAndRecovers(t *testing.T) {
	for _, kind := range []string{"directory", "reparse"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "host-state")
			store, _, err := Open(Config{Root: root, Clock: func() time.Time { return testNow }})
			if err != nil {
				t.Fatal("store initialization failed")
			}
			if _, err := store.Commit(1, validState(t, 2, 1)); err != nil {
				store.Close()
				t.Fatal("fixture commit failed")
			}
			if err := store.Close(); err != nil {
				t.Fatal("fixture close failed")
			}
			primaryPath, backupPath := filepath.Join(root, primaryFile), filepath.Join(root, backupFile)
			primary, err := os.ReadFile(primaryPath)
			if err != nil {
				t.Fatal("primary fixture read failed")
			}
			backup, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatal("backup fixture read failed")
			}
			if err := os.Remove(backupPath); err != nil {
				t.Fatal("owned backup removal failed")
			}
			if kind == "directory" {
				if err := os.Mkdir(backupPath, 0700); err != nil {
					t.Fatal("invalid directory fixture failed")
				}
			} else {
				target := filepath.Join(root, "owned-backup-target")
				if err := os.WriteFile(target, backup, 0600); err != nil {
					t.Fatal("owned reparse target failed")
				}
				if err := os.Symlink(target, backupPath); err != nil {
					t.Fatal("actual reparse fixture unavailable")
				}
			}
			opened, status, err := Open(Config{Root: root})
			if opened != nil {
				opened.Close()
				t.Fatal("invalid backup authorized open or fallback")
			}
			if !errors.Is(err, ErrInvalidState) || errors.Is(err, ErrCorrupt) || status.Code != "backup_unreadable" || status.Source != "none" || len(status.PreservedPaths) != 0 {
				t.Fatal("invalid backup did not fail closed with original cause")
			}
			unchanged, readErr := os.ReadFile(primaryPath)
			if readErr != nil || !bytes.Equal(unchanged, primary) {
				t.Fatal("invalid backup changed primary")
			}
			info, err := os.Lstat(backupPath)
			if err != nil || (kind == "directory" && !info.IsDir()) || (kind == "reparse" && info.Mode()&os.ModeSymlink == 0) {
				t.Fatal("invalid backup was replaced")
			}
			if err := os.Remove(backupPath); err != nil {
				t.Fatal("owned invalid backup removal failed")
			}
			if err := os.WriteFile(backupPath, backup, 0600); err != nil {
				t.Fatal("original backup restoration failed")
			}
			recovered, _, err := Open(Config{Root: root})
			if err != nil {
				t.Fatal("original protected backup did not recover")
			}
			defer recovered.Close()
			state, revision, err := recovered.Snapshot()
			if err != nil || revision != 2 || len(state.Tunnels) != 1 || state.Tunnels[0].LastKnownGood == nil || state.Tunnels[0].LastKnownGood.Generation != 1 {
				t.Fatal("recovery changed generation or private LKG")
			}
		})
	}
}
