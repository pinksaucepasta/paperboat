//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func plantJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, output)
	}
}

// Security review, 2026-11-10: the elevated uninstall helper removes user-writable paths. A junction planted in
// or above one turned that into an administrator's delete of whatever it points at.
func TestWindowsUserStateRemovalNeverFollowsAPlantedJunction(t *testing.T) {
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "keep", "sentinel.txt")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("not Paperboat's"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	survived := func(t *testing.T) {
		t.Helper()
		if body, err := os.ReadFile(sentinel); err != nil || string(body) != "not Paperboat's" {
			t.Fatalf("the file behind the junction was touched: %v", err)
		}
	}

	t.Run("inside the state root it is removed as an entry", func(t *testing.T) {
		state := filepath.Join(profile, "AppData", "Local", "Paperboat")
		plantJunction(t, filepath.Join(state, "runtime"), outside)
		if err := os.WriteFile(filepath.Join(state, "credentials.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := retryWindowsRemoval(ctx, profile, state, nil); err != nil {
			t.Fatalf("remove a state root that holds a junction: %v", err)
		}
		if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state root is still there: %v", err)
		}
		survived(t)
	})
	t.Run("beside a preserved Inbox it is removed as an entry", func(t *testing.T) {
		state := filepath.Join(profile, "Paperboat")
		inbox := filepath.Join(state, "Inbox")
		if err := os.MkdirAll(inbox, 0o700); err != nil {
			t.Fatal(err)
		}
		received := filepath.Join(inbox, "received.txt")
		if err := os.WriteFile(received, []byte("user file"), 0o600); err != nil {
			t.Fatal(err)
		}
		plantJunction(t, filepath.Join(state, "link"), outside)
		if err := retryWindowsRemoval(ctx, profile, state, []string{inbox}); err != nil {
			t.Fatalf("remove a state root that holds the Inbox and a junction: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(state, "link")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the junction is still there: %v", err)
		}
		if _, err := os.Lstat(received); err != nil {
			t.Fatalf("the Inbox was not preserved: %v", err)
		}
		survived(t)
	})
	for name, path := range map[string]string{
		"the state root itself":            filepath.Join(profile, "AppData", "Roaming", "Paperboat"),
		"a directory above the state root": filepath.Join(profile, "Linked", "keep"),
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			link := path
			if filepath.Base(path) == "keep" {
				link = filepath.Dir(path)
			}
			plantJunction(t, link, outside)
			if err := retryWindowsRemoval(ctx, profile, path, nil); !errors.Is(err, errWindowsUserStateRefused) {
				t.Fatalf("a path through a junction: %v, want the refusal", err)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Fatalf("the refused junction was changed: %v", err)
			}
			survived(t)
		})
	}
	t.Run("outside the profile is refused", func(t *testing.T) {
		for _, path := range []string{filepath.Dir(sentinel), profile, filepath.Dir(profile)} {
			if err := retryWindowsRemoval(ctx, profile, path, nil); !errors.Is(err, errWindowsUserStateRefused) {
				t.Fatalf("%s: %v, want the refusal", path, err)
			}
		}
		survived(t)
	})
}
