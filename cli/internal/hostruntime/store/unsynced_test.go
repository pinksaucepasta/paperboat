package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestUnsyncedRestoresFullSync(t *testing.T) {
	state, err := Open(context.Background(), Config{Root: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	mode := func() (value int) {
		if err := state.db.QueryRow("PRAGMA synchronous").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	ctx, cancel := context.WithCancel(context.Background())
	failed := errors.New("failed")
	if err := state.Unsynced(ctx, func() error {
		if mode() != 1 {
			t.Error("work runs with a sync per commit")
		}
		cancel()
		return failed
	}); err != failed {
		t.Fatal("the work's error was dropped")
	}
	if mode() != 2 {
		t.Fatal("later commits are not synced")
	}
}
