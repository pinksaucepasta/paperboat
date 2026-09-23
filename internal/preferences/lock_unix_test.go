//go:build darwin || linux

package preferences

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPreferenceLockRejectsConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	_, unlock, err := lockPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, otherUnlock, err := lockPreferences(path); !errors.Is(err, ErrBusy) {
		if otherUnlock != nil {
			_ = otherUnlock()
		}
		t.Fatalf("second lock err=%v", err)
	}
}

func TestPreferenceLockRejectsOtherProcess(t *testing.T) {
	if path := os.Getenv("PB_PREFERENCES_LOCK_HELPER"); path != "" {
		_, unlock, err := lockPreferences(path)
		if err != nil {
			os.Exit(2)
		}
		defer unlock()
		_ = os.WriteFile(path+".ready", []byte("ready"), 0600)
		for {
			if _, err := os.Stat(path + ".release"); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	path := filepath.Join(t.TempDir(), "preferences.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPreferenceLockRejectsOtherProcess$")
	cmd.Env = append(os.Environ(), "PB_PREFERENCES_LOCK_HELPER="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile(path+".release", []byte("release"), 0600); _ = cmd.Wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, unlock, err := lockPreferences(path); !errors.Is(err, ErrBusy) {
		if unlock != nil {
			_ = unlock()
		}
		t.Fatalf("cross-process lock err=%v", err)
	}
}
