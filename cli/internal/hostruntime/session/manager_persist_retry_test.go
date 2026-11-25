package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// The ring moved past the stored position while writes failed. The store keeps the 100 stored chunks until the
// transaction that adds the whole newer ring, then holds what the ring holds (both are bounded by HistoryBytes);
// a reader behind it gets the replay gap, also after a restart.
func TestOutputPersistenceReplacesStoredOutputAtomicallyAfterOverflow(t *testing.T) {
	const bound = 4096
	var failing atomic.Bool
	state, manager, session, _ := retrySession(t, store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
		if failing.Load() && point == "append_before_commit" {
			return errors.New("disk full")
		}
		return nil
	}})
	ring, err := history.New(bound)
	if err != nil {
		t.Fatal(err)
	}
	manager.config.HistoryBytes = bound
	session.history = ring
	put := func(prefix string, count int) {
		for i := range count {
			if _, err := ring.Append(1, []byte(fmt.Sprintf("%s-%03d\n", prefix, i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	writer := newOutputWriter(manager)
	manager.writerOnce.Do(func() { manager.output = writer }) // commits below run by hand
	manager.startOutputPersistence(session)
	put("old", 100) // 800 bytes
	writer.mark(session)
	writer.commit()
	failing.Store(true)
	put("new", 1000) // 8000 bytes: the ring has moved past every stored chunk
	writer.mark(session)
	writer.commit()
	stored := func() (text string, earliest uint64) {
		_, latestRow, err := state.OutputBounds(t.Context(), session.id)
		if err != nil {
			t.Fatal(err)
		}
		earliest, _, _ = state.OutputBounds(t.Context(), session.id)
		events, _, _, err := state.Replay(t.Context(), session.id, earliest, latestRow)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			text += string(event.Data)
		}
		return text, earliest
	}
	if text, _ := stored(); len(text) != 800 || !strings.HasPrefix(text, "old-000") {
		t.Fatalf("a failing store lost stored output: %d bytes", len(text))
	}
	failing.Store(false)
	session.persistMu.Lock()
	session.persistRetryAt = time.Time{}
	session.persistMu.Unlock()
	item, ok := writer.collect(session, 16) // the group commit's budget is almost spent
	if !ok {
		t.Fatal("nothing to write after recovery")
	}
	if err := state.AppendOutputBatch(t.Context(), []store.OutputBatch{item.batch}, bound); err != nil {
		t.Fatal(err)
	}
	item.replay.Release()
	live, err := ring.Replay(item.batch.AdvanceTo, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	for _, event := range live.Events {
		want += string(event.Data)
	}
	text, earliest := stored()
	if text != want || len(text) < bound-8 || len(text) > bound {
		t.Fatalf("one transaction left %d stored bytes, the ring holds %d", len(text), len(want))
	}
	restarted, err := NewManager(ManagerConfig{Store: state, HistoryBytes: bound, AttachmentBytes: 1 << 20, PreserveStoppedHistory: true, Launch: func(pty.Command) (PTYProcess, error) { return nil, errors.New("no launch") }})
	if err != nil {
		t.Fatal(err)
	}
	var gap *history.GapError
	if _, err := restarted.Attach(session.id, "att_behind", 800); !errors.As(err, &gap) || gap.EarliestSequence != earliest {
		t.Fatalf("a reader behind the lost span got %v, want the replay gap", err)
	}
	attached, err := restarted.Attach(session.id, "att_rejoin", earliest)
	if err != nil {
		t.Fatal(err)
	}
	replayed := ""
	for _, event := range attached.Replay.Events {
		replayed += string(event.Data)
	}
	if replayed != want {
		t.Fatalf("replay after restart has %d bytes, want %d", len(replayed), len(want))
	}
}

// A refused write stays visible in the counts doctor reads for as long as it lasts, and a stopped session
// leaves no retry timer behind.
func TestUnsavedOutputStaysCountedAndCloseDropsTheRetry(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	_, manager, session, print := retrySession(t, store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
		if failing.Load() && point == "append_before_commit" {
			return errors.New("disk full")
		}
		return nil
	}})
	session.process, session.fanout = &heldProcess{}, NewFanout()
	manager.sessions = map[string]*managedSession{session.id: session}
	manager.startOutputPersistence(session)
	print("text")
	waitPersist(t, session, "the failure did not last", func() bool { return session.persistFailures >= 3 })
	if got := manager.ResourceCounts()["unsaved_outputs"]; got != 1 {
		t.Fatalf("unsaved_outputs=%d while writes fail", got)
	}
	failing.Store(false)
	waitPersist(t, session, "not recovered", func() bool { return session.persistErr == nil })
	if got := manager.ResourceCounts()["unsaved_outputs"]; got != 0 {
		t.Fatalf("unsaved_outputs=%d after recovery", got)
	}
	failing.Store(true)
	print("more")
	if err := manager.stopOutputPersistence(session); err == nil {
		t.Fatal("close hid the failed write")
	}
	session.persistMu.Lock()
	defer session.persistMu.Unlock()
	if session.persistRetry != nil {
		t.Fatal("a retry timer outlived the close")
	}
}

type heldProcess struct{ PTYProcess }

// An ambiguous commit stored only the first part of one ring chunk (a chunk larger than a group commit), so the
// position read back lies inside that chunk. The retry continues from there: no byte twice, none missing.
func TestOutputPersistenceResumesInsideAChunk(t *testing.T) {
	var landed atomic.Bool
	state, manager, session, print := retrySession(t, store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
		if point == "append_after_commit" && landed.CompareAndSwap(false, true) {
			return errors.New("commit reported a failure")
		}
		return nil
	}})
	chunk := make([]byte, outputCommitBytes+50_000)
	for i := range chunk {
		chunk[i] = byte('a' + i%23)
	}
	want := string(chunk) + "tail"
	manager.startOutputPersistence(session)
	print(string(chunk))
	waitPersist(t, session, "the rest of the chunk was not written", func() bool {
		return session.persistErr == nil && session.persistedLatest == uint64(len(chunk))
	})
	if !landed.Load() {
		t.Fatal("no commit was made ambiguous")
	}
	print("tail")
	if err := manager.stopOutputPersistence(session); err != nil {
		t.Fatal(err)
	}
	manager.stopWriter()
	restarted, err := NewManager(ManagerConfig{Store: state, HistoryBytes: 1 << 20, AttachmentBytes: 1 << 20, PreserveStoppedHistory: true, Launch: func(pty.Command) (PTYProcess, error) { return nil, errors.New("no launch") }})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.get(session.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []uint64{0, outputCommitBytes} { // outputCommitBytes is where the partial commit ended
		stored, _, _, err := state.Replay(t.Context(), session.id, from, 0)
		if err != nil {
			t.Fatal(err)
		}
		var durable, next = "", from
		for _, event := range stored {
			if event.StartSequence != next {
				t.Fatalf("stored output from %d jumps from %d to %d", from, next, event.StartSequence)
			}
			durable, next = durable+string(event.Data), event.EndSequence
		}
		if durable != want[from:] {
			t.Fatalf("stored output from %d: %d bytes, want %d", from, len(durable), len(want)-int(from))
		}
		for name, ring := range map[string]*history.History{"live": session.history, "restarted": recovered.history} {
			replay, err := ring.Replay(from, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, event := range replay.Events {
				got += string(event.Data)
			}
			if got != want[from:] {
				t.Fatalf("%s replay from %d: %d bytes, want %d", name, from, len(got), len(want)-int(from))
			}
		}
	}
}
