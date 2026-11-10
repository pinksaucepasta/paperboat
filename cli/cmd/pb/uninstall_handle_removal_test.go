package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Security review, 2026-11-10: the part of the Windows uninstall that runs with administrator rights in a
// directory the user can write must not resolve a path or follow a link.
func TestRemoveBelowDirectoryFollowsNoLinkAndKeepsTheInbox(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(base, "outside", "sentinel.txt")
	state := filepath.Join(base, "profile", "Paperboat")
	inbox := filepath.Join(state, "Inbox")
	received := filepath.Join(inbox, "received.txt")
	build := func(t *testing.T) *os.Root {
		t.Helper()
		for _, file := range []string{sentinel, received, filepath.Join(state, "credentials.json"), filepath.Join(state, "runtime", "store", "state.json")} {
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("kept"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, link := range []string{filepath.Join(state, "link"), filepath.Join(state, "runtime", "link")} {
			if err := os.Symlink(filepath.Dir(sentinel), link); err != nil && !errors.Is(err, os.ErrExist) {
				t.Skipf("symbolic links are not available: %v", err)
			}
		}
		parent, err := os.OpenRoot(filepath.Dir(state))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = parent.Close() })
		return parent
	}
	survived := func(t *testing.T, path string) {
		t.Helper()
		if body, err := os.ReadFile(path); err != nil || string(body) != "kept" {
			t.Fatalf("%s was touched: %v", path, err)
		}
	}
	accept := func(*os.Root, string) error { return nil }

	t.Run("a state directory is removed whole, links as entries", func(t *testing.T) {
		parent := build(t)
		if err := removeBelowDirectory(parent, "Paperboat", state, nil, accept); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state directory is still there: %v", err)
		}
		survived(t, sentinel)
	})
	t.Run("everything but the Inbox is removed", func(t *testing.T) {
		parent := build(t)
		var entered []string
		check := func(_ *os.Root, path string) error { entered = append(entered, path); return nil }
		if err := removeBelowDirectory(parent, "Paperboat", state, []string{inbox}, check); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(state)
		if err != nil || len(entries) != 1 || entries[0].Name() != "Inbox" {
			t.Fatalf("left in the state directory: %v, %v; want only the Inbox", entries, err)
		}
		survived(t, received)
		survived(t, sentinel)
		// Every directory is checked before it is emptied; a link and the Inbox are never entered.
		want := []string{state, filepath.Join(state, "runtime"), filepath.Join(state, "runtime", "store")}
		if !slices.Equal(entered, want) {
			t.Fatalf("checked directories = %v, want %v", entered, want)
		}
	})
	t.Run("a nested directory the check refuses stays with its content", func(t *testing.T) {
		parent := build(t)
		refused := errors.New("refused")
		store := filepath.Join(state, "runtime", "store")
		err := removeBelowDirectory(parent, "Paperboat", state, nil, func(_ *os.Root, path string) error {
			if path == store {
				return refused
			}
			return nil
		})
		if !errors.Is(err, refused) {
			t.Fatalf("a refused nested directory: %v, want the refusal", err)
		}
		survived(t, filepath.Join(store, "state.json"))
		for _, gone := range []string{filepath.Join(state, "credentials.json"), filepath.Join(state, "link"), filepath.Join(state, "runtime", "link")} {
			if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s was not removed: %v", gone, err)
			}
		}
		survived(t, sentinel)
		if err := os.RemoveAll(state); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a directory the check refuses is not touched", func(t *testing.T) {
		parent := build(t)
		refused := errors.New("refused")
		err := removeBelowDirectory(parent, "Paperboat", state, []string{inbox}, func(*os.Root, string) error { return refused })
		if !errors.Is(err, refused) {
			t.Fatalf("a refused directory: %v, want the refusal", err)
		}
		survived(t, filepath.Join(state, "credentials.json"))
		survived(t, received)
	})
	t.Run("the Inbox itself is kept", func(t *testing.T) {
		parent := build(t)
		inboxParent, err := parent.OpenRoot("Paperboat")
		if err != nil {
			t.Fatal(err)
		}
		defer inboxParent.Close()
		if err := removeBelowDirectory(inboxParent, "Inbox", inbox, []string{inbox}, accept); err != nil {
			t.Fatal(err)
		}
		survived(t, received)
	})
}
