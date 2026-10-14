package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenReadOnlyMetadataPreservesExistingStore(t *testing.T) {
	ctx := context.Background()
	state, root := openStore(t, nil)
	defer state.Close()

	session := testSession()
	session.CommandPath = "/bin/sh"
	session.CommandArgs = []string{"-c", "echo private"}
	session.CommandEnv = []string{"PAPERBOAT_TEST_SECRET=private"}
	if err := state.CreateSession(ctx, session); err != nil {
		t.Fatal("create session:", err)
	}
	var argsBytes, envBytes int
	if err := state.db.QueryRowContext(ctx, `SELECT length(command_args),length(command_env) FROM sessions WHERE id=?`, session.ID).Scan(&argsBytes, &envBytes); err != nil {
		t.Fatal("read persisted command metadata lengths:", err)
	}
	if argsBytes == 0 || envBytes == 0 {
		t.Fatal("fixture did not persist command arguments and environment")
	}

	databasePath := filepath.Join(root, "state.db")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(databasePath, 0o640); err != nil {
			t.Fatal("set sentinel database mode:", err)
		}
	}
	beforeDatabase := readOnlyTestFile(t, databasePath)
	walPath := databasePath + "-wal"
	beforeWAL := readOnlyTestFile(t, walPath)
	if len(beforeWAL) == 0 {
		t.Fatal("fixture has no pending WAL frames to protect from checkpointing")
	}
	beforeMode, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal("stat database:", err)
	}

	reader, err := OpenReadOnly(ctx, root)
	if err != nil {
		t.Fatal("open read-only store:", err)
	}
	metadata, err := reader.BoundedSessionMetadata(ctx, 1, readOnlyMetadataSize(session))
	if err != nil {
		t.Fatal("read bounded metadata:", err)
	}
	if len(metadata) != 1 || metadata[0].ID != session.ID || metadata[0].Name != session.Name || metadata[0].CWD != session.CWD {
		t.Fatal("read-only metadata did not return the persisted session")
	}
	if metadata[0].CommandPath != "" || len(metadata[0].CommandArgs) != 0 || len(metadata[0].CommandEnv) != 0 {
		t.Fatal("read-only metadata exposed command details")
	}
	if _, err := reader.BoundedSessionMetadata(ctx, 1, readOnlyMetadataSize(session)-1); !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("undersized metadata byte limit error = %v, want ErrResultTooLarge", err)
	}
	writeErr := reader.CreateSession(ctx, Session{
		ID: "ses_read_only_write", Name: "read-only-write", CWD: "/tmp",
		Columns: 80, Rows: 24, State: "running", Generation: 1,
	})
	if writeErr == nil {
		t.Fatal("read-only store accepted a session write")
	}
	metadata, err = reader.BoundedSessionMetadata(ctx, 1, readOnlyMetadataSize(session))
	if err != nil || len(metadata) != 1 || metadata[0].ID != session.ID {
		t.Fatal("failed write changed the original session state")
	}
	if err := reader.Close(); err != nil {
		t.Fatal("close read-only store:", err)
	}

	assertReadOnlyTestFileUnchanged(t, databasePath, beforeDatabase)
	assertReadOnlyTestFileUnchanged(t, walPath, beforeWAL)
	if runtime.GOOS != "windows" {
		afterMode, err := os.Stat(databasePath)
		if err != nil {
			t.Fatal("stat database after read:", err)
		}
		if beforeMode.Mode().Perm() != afterMode.Mode().Perm() {
			t.Fatalf("database permissions changed from %04o to %04o", beforeMode.Mode().Perm(), afterMode.Mode().Perm())
		}
	}
}

func TestOpenReadOnlyRejectsMissingAndInvalidStoresWithoutRepair(t *testing.T) {
	t.Run("missing database remains missing", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal("create store directory:", err)
		}
		if _, err := OpenReadOnly(context.Background(), root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing database error = %v, want os.ErrNotExist", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "state.db")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("database after failed open error = %v, want os.ErrNotExist", err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("store directory entries after failed open = %d, err = %v", len(entries), err)
		}
	})

	t.Run("incompatible version is not migrated", func(t *testing.T) {
		state, root := openStore(t, nil)
		defer state.Close()
		if _, err := state.db.ExecContext(context.Background(), "PRAGMA user_version=2"); err != nil {
			t.Fatal("set incompatible version:", err)
		}
		if err := state.Close(); err != nil {
			t.Fatal("close fixture store:", err)
		}
		databasePath := filepath.Join(root, "state.db")
		before := readOnlyTestFile(t, databasePath)
		if _, err := OpenReadOnly(context.Background(), root); !errors.Is(err, ErrIncompatible) {
			t.Fatalf("incompatible database error = %v, want ErrIncompatible", err)
		}
		assertReadOnlyTestFileUnchanged(t, databasePath, before)
	})

	t.Run("schema drift is not repaired", func(t *testing.T) {
		state, root := openStore(t, nil)
		defer state.Close()
		if _, err := state.db.ExecContext(context.Background(), `CREATE TABLE unexpected_state(value TEXT)`); err != nil {
			t.Fatal("create schema drift:", err)
		}
		if err := state.Close(); err != nil {
			t.Fatal("close fixture store:", err)
		}
		databasePath := filepath.Join(root, "state.db")
		before := readOnlyTestFile(t, databasePath)
		if _, err := OpenReadOnly(context.Background(), root); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("invalid schema error = %v, want ErrCorrupt", err)
		}
		assertReadOnlyTestFileUnchanged(t, databasePath, before)
	})
}

func TestBoundedSessionMetadataRejectsOversizeBeforeScanning(t *testing.T) {
	testCases := []struct {
		name         string
		maximum      int
		maximumBytes int
		addSecondRow bool
	}{
		{name: "count", maximum: 1, maximumBytes: 1024, addSecondRow: true},
		{name: "bytes", maximum: 1, maximumBytes: 1},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			state, root := openStore(t, nil)
			defer state.Close()
			first := testSession()
			if err := state.CreateSession(ctx, first); err != nil {
				t.Fatal("create first session:", err)
			}
			if testCase.addSecondRow {
				second := testSession()
				second.ID, second.Name = "ses_2", "second"
				if err := state.CreateSession(ctx, second); err != nil {
					t.Fatal("create second session:", err)
				}
			}
			// Generation zero is valid SQLite state but invalid session metadata.
			// The applicable size bound must win before records are scanned.
			invalidID := first.ID
			if testCase.addSecondRow {
				invalidID = "ses_2"
			}
			if _, err := state.db.ExecContext(ctx, `UPDATE sessions SET generation=0 WHERE id=?`, invalidID); err != nil {
				t.Fatal("make metadata row invalid:", err)
			}
			reader, err := OpenReadOnly(ctx, root)
			if err != nil {
				t.Fatal("open read-only store:", err)
			}
			defer reader.Close()
			if _, err := reader.BoundedSessionMetadata(ctx, testCase.maximum, testCase.maximumBytes); !errors.Is(err, ErrResultTooLarge) {
				t.Fatalf("bounded metadata error = %v, want ErrResultTooLarge", err)
			}
		})
	}
}

func readOnlyMetadataSize(session Session) int {
	return len(session.ID) + len(session.Name) + len(session.CWD) + len(session.ExitSignal)
}

func readOnlyTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read store file:", err)
	}
	return data
}

func assertReadOnlyTestFileUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	after := readOnlyTestFile(t, path)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only store changed a database file")
	}
}
