package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
)

func retrySession(t *testing.T, config store.Config) (*store.Store, *Manager, *managedSession, func(string)) {
	t.Helper()
	state, err := store.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	now := time.Now().UTC()
	exit := 0
	if err := state.CreateSession(t.Context(), store.Session{ID: "ses_retry", Name: "retry", CWD: t.TempDir(), CommandPath: "/bin/sh", CommandArgs: []string{"-c", "exit"}, CommandEnv: []string{"PATH=/bin"}, Columns: 80, Rows: 24, State: string(Exited), Generation: 1, ExitCode: &exit, ExitedAt: &now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ring, err := history.New(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: ManagerConfig{Store: state, HistoryBytes: 1 << 20}}
	t.Cleanup(manager.stopWriter)
	session := &managedSession{id: "ses_retry", history: ring}
	print := func(text string) {
		event, err := ring.Append(1, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		manager.queueOutputPersistence(session, event)
	}
	return state, manager, session, print
}

func waitPersist(t *testing.T, session *managedSession, what string, reached func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		session.persistMu.Lock()
		ok := reached()
		session.persistMu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// One write the store refused (SQLITE_BUSY past busy_timeout) ended a session's persistence for life,
// so a restart replayed a gap. "start" loses the store before the position to write from was ever read.
func TestOutputPersistenceResumesAfterBusyStore(t *testing.T) {
	for _, when := range []string{"write", "start"} {
		t.Run(when, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			state, manager, session, print := retrySession(t, store.Config{Root: root})
			want := ""
			out := func(text string) { want += text; print(text) }
			manager.startOutputPersistence(session)
			if when == "start" { // what startOutputPersistence does when OutputBounds fails
				manager.writer().failed(session, errors.New("bounds unavailable"))
			}
			out("before ")
			db, err := sql.Open("sqlite", filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			holder, err := db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			if _, err := holder.ExecContext(t.Context(), "PRAGMA busy_timeout = 10000"); err != nil {
				t.Fatal(err)
			}
			if _, err := holder.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			out("during ")
			waitPersist(t, session, "no write failed while the store was held", func() bool { return store.Busy(session.persistErr) })
			out("held ")
			if _, err := holder.ExecContext(t.Context(), "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			waitPersist(t, session, "the missed output was not written after the store recovered", func() bool {
				return session.persistErr == nil && session.persistedLatest == uint64(len(want))
			})
			out("after")
			if err := manager.stopOutputPersistence(session); err != nil {
				t.Fatal(err)
			}
			manager.stopWriter()
			recovered, err := NewManager(ManagerConfig{Store: state, HistoryBytes: 1 << 20, AttachmentBytes: 1 << 20, PreserveStoppedHistory: true, Launch: func(pty.Command) (PTYProcess, error) { return nil, errors.New("no launch") }})
			if err != nil {
				t.Fatal(err)
			}
			attached, err := recovered.Attach("ses_retry", "att_replay", 0)
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			for _, event := range attached.Replay.Events {
				got.Write(event.Data)
			}
			if got.String() != want {
				t.Fatalf("replay %q, want %q", got.String(), want)
			}
		})
	}
}

// A commit that landed but reported an error is neither written twice nor retried into a conflict.
func TestOutputPersistenceReadsItsPositionAfterAnAmbiguousCommit(t *testing.T) {
	landed := false
	state, manager, session, print := retrySession(t, store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
		if point == "append_after_commit" && !landed {
			landed = true
			return errors.New("commit reported a failure")
		}
		return nil
	}})
	manager.startOutputPersistence(session)
	print("one")
	waitPersist(t, session, "position not recovered", func() bool { return session.persistErr == nil && session.persistedLatest == 3 })
	print("two")
	if err := manager.stopOutputPersistence(session); err != nil {
		t.Fatal(err)
	}
	if _, earliest, latest, err := state.Replay(context.Background(), session.id, 0, 0); err != nil || earliest != 0 || latest != 6 {
		t.Fatalf("bounds=[%d,%d) err=%v", earliest, latest, err)
	}
}

// A closed store can never take the output: the session stops with the cause instead of retrying forever.
func TestOutputPersistenceStopsOnAClosedStore(t *testing.T) {
	state, manager, session, print := retrySession(t, store.Config{Root: filepath.Join(t.TempDir(), "state")})
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	manager.startOutputPersistence(session)
	print("lost")
	session.persistMu.Lock()
	persisting, cause := session.persisting, session.persistErr
	session.persistMu.Unlock()
	if persisting || cause == nil {
		t.Fatalf("persisting=%v err=%v on a closed store", persisting, cause)
	}
	if err := manager.stopOutputPersistence(session); err == nil {
		t.Fatal("close hid the cause")
	}
}
