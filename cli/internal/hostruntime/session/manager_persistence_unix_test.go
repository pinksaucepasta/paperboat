//go:build darwin || linux

package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func TestManagerRecoversHistoryInputAndRestartGeneration(t *testing.T) {
	workspace := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "state")
	adapter, err := pty.NewAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	open := func() *store.Store {
		state, err := store.Open(context.Background(), store.Config{Root: stateRoot})
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	newManager := func(state *store.Store, random []byte) *Manager {
		manager, err := NewManager(ManagerConfig{Store: state, Launch: func(command pty.Command) (PTYProcess, error) { return adapter.Start(command) }, Random: bytes.NewReader(random), HistoryBytes: 1 << 20, AttachmentBytes: 1 << 20, TerminationTimeout: 3 * time.Second, TerminationGrace: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	random := make([]byte, 64)
	for i := range random {
		random[i] = byte(i)
	}
	state := open()
	manager := newManager(state, random)
	command := shellCommand("/bin/sh", workspace, "printf abc; read line; printf done; exit 7")
	created, err := manager.Create(context.Background(), CreateRequest{ID: "terminal_00000000-0000-4000-8000-000000000001", Name: "default", Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "terminal_00000000-0000-4000-8000-000000000001" {
		t.Fatalf("runtime changed authoritative terminal identity: %s", created.ID)
	}
	waitLatest(t, manager, created.ID, 3)
	if _, err := manager.Attach(created.ID, "att_1", 0); err != nil {
		t.Fatal(err)
	}
	key := InputKey{ClientID: "cli", AttachmentID: "att_1", Generation: created.Generation, InputID: "inp_0001"}
	if decision, err := manager.Write(created.ID, key, []byte("go\n")); err != nil || decision.Status != InputAccepted {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	waitState(t, manager, created.ID, Exited)
	// The completed terminal keeps its cursor but clears its replay output.
	waitLatest(t, manager, created.ID, 7)
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	state = open()
	defer state.Close()
	recoveredManager := newManager(state, random)
	recovered, err := recoveredManager.Snapshot(created.ID)
	if err != nil || recovered.State != Exited || recovered.Generation != 1 || recovered.Exit == nil || recovered.Exit.Code != 7 {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	if recovered.EarliestSequence != recovered.LatestSequence {
		t.Fatalf("closed terminal retained replay: %#v", recovered)
	}
	attached, err := recoveredManager.Attach(created.ID, "att_2", recovered.LatestSequence)
	if err != nil || len(attached.Replay.Events) != 0 {
		t.Fatalf("attach=%#v err=%v", attached, err)
	}
	if decision, err := recoveredManager.QueryInput(created.ID, key); err != nil || decision.Status != InputAccepted {
		t.Fatalf("query=%#v err=%v", decision, err)
	}
	restarted, err := recoveredManager.Restart(created.ID)
	if err != nil || restarted.ID != created.ID || restarted.Generation != 2 {
		t.Fatalf("restart=%#v err=%v", restarted, err)
	}
	if _, err := recoveredManager.Close(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
}

func TestManagerLiveOutputDoesNotWaitForSQLitePersistence(t *testing.T) {
	workspace := t.TempDir()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releasePersistence := func() { releaseOnce.Do(func() { close(release) }) }
	defer releasePersistence()
	state, err := store.Open(context.Background(), store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
		if point == "append_before_commit" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	adapter, err := pty.NewAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		Store:  state,
		Launch: func(command pty.Command) (PTYProcess, error) { return adapter.Start(command) },
		Random: bytes.NewReader(make([]byte, 64)), HistoryBytes: 64 << 10, AttachmentBytes: 1 << 20,
		TerminationTimeout: 3 * time.Second, TerminationGrace: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Name: "nonblocking", Command: shellCommand("/bin/sh", workspace, "read line; printf live-output; read line")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AttachLive(created.ID, "att_live"); err != nil {
		t.Fatal(err)
	}
	decision, err := manager.Write(created.ID, InputKey{ClientID: "cli", AttachmentID: "att_live", Generation: created.Generation, InputID: "inp_live"}, []byte("go\n"))
	if err != nil || decision.Status != InputAccepted {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("persistence worker did not enter blocked commit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var output []byte
	for !bytes.Contains(output, []byte("live-output")) {
		event, err := manager.WaitNext(ctx, created.ID, "att_live")
		if err != nil {
			t.Fatalf("output=%q err=%v", output, err)
		}
		output = append(output, event.Data...)
	}
	releasePersistence()
	closed, err := manager.Close(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.EarliestSequence != closed.LatestSequence {
		t.Fatalf("closed terminal retained replay bounds: %#v", closed)
	}
	stored, earliest, latest, err := state.Replay(context.Background(), created.ID, closed.LatestSequence, 0)
	if err != nil || len(stored) != 0 || earliest != latest {
		t.Fatalf("closed terminal retained stored output: events=%d earliest=%d latest=%d err=%v", len(stored), earliest, latest, err)
	}
}

func TestManagerPersistenceBacklogRetainsOnlyBoundedHistory(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var commits atomic.Int64
	state, err := store.Open(context.Background(), store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
		if point != "append_before_commit" {
			return nil
		}
		if commits.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	now := time.Now().UTC()
	if err := state.CreateSession(context.Background(), store.Session{ID: "ses_backlog", Name: "backlog", CWD: t.TempDir(), CommandPath: "/bin/sh", CommandArgs: []string{"-c", "exit"}, CommandEnv: []string{"PATH=/bin"}, Columns: 80, Rows: 24, State: "running", Generation: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	retained, err := history.New(64)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: ManagerConfig{Store: state, HistoryBytes: 64}}
	session := &managedSession{id: "ses_backlog", history: retained}
	manager.startOutputPersistence(session)
	appendByte := func(value byte) {
		buffer := history.AcquireBuffer()
		buffer[0] = value
		event, err := retained.AppendBuffer(1, buffer[:1])
		if err != nil {
			t.Fatal(err)
		}
		manager.queueOutputPersistence(session, event)
		event.Release()
	}
	appendByte(0)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("persistence worker did not enter blocked commit")
	}
	for index := 1; index <= 1000; index++ {
		appendByte(byte(index))
	}
	close(release)
	if err := manager.stopOutputPersistence(session); err != nil {
		t.Fatal(err)
	}
	earliest, latest, err := state.OutputBounds(context.Background(), session.id)
	if err != nil || earliest != latest-64 || latest != 1001 {
		t.Fatalf("bounds=[%d,%d) err=%v", earliest, latest, err)
	}
	if got := commits.Load(); got > 65 {
		t.Fatalf("persisted %d events for a 64-byte retained tail", got)
	}
}

func TestManagerMarksUnverifiedRunningGenerationLostOnRecovery(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	state, err := store.Open(context.Background(), store.Config{Root: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := store.Session{ID: "ses_lost", Name: "lost", CWD: t.TempDir(), CommandPath: "/bin/sh", CommandArgs: []string{"-c", "exit"}, CommandEnv: []string{"PATH=/bin"}, Columns: 80, Rows: 24, State: "running", Generation: 3, CreatedAt: now, UpdatedAt: now}
	if err := state.CreateSession(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = store.Open(context.Background(), store.Config{Root: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	manager, err := NewManager(ManagerConfig{Store: state, Launch: func(pty.Command) (PTYProcess, error) { t.Fatal("recovery must not launch"); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot("ses_lost")
	if err != nil || snapshot.State != Exited || snapshot.Generation != 3 || snapshot.Exit == nil || snapshot.Exit.Signal != "runtime_restart" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestManagerMarksRunningGenerationLostOnMachineReboot(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	state, err := store.Open(context.Background(), store.Config{Root: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := store.Session{ID: "ses_reboot", Name: "reboot", CWD: t.TempDir(), CommandPath: "/bin/sh", CommandArgs: []string{"-c", "exit"}, CommandEnv: []string{"PATH=/bin"}, Columns: 80, Rows: 24, State: "restarting", Generation: 4, CreatedAt: now, UpdatedAt: now}
	if err := state.CreateSession(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{Store: state, RecoveryExitSignal: "machine_reboot", Launch: func(pty.Command) (PTYProcess, error) { t.Fatal("recovery must not launch"); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	snapshot, err := manager.Snapshot("ses_reboot")
	if err != nil || snapshot.State != Exited || snapshot.Generation != 4 || snapshot.Exit == nil || snapshot.Exit.Signal != "machine_reboot" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestManagerShutdownPreservesRunningGenerationForBootRecovery(t *testing.T) {
	workspace := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "state")
	state, err := store.Open(context.Background(), store.Config{Root: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := pty.NewAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{Store: state, Launch: func(command pty.Command) (PTYProcess, error) { return adapter.Start(command) }, TerminationTimeout: 3 * time.Second, TerminationGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Name: "boot-recovery", Command: shellCommand("/bin/sh", workspace, "printf retained; read line")})
	if err != nil {
		t.Fatal(err)
	}
	waitLatest(t, manager, created.ID, uint64(len("retained")))
	if err := manager.ShutdownForRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = store.Open(context.Background(), store.Config{Root: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	recovered, err := NewManager(ManagerConfig{Store: state, RecoveryExitSignal: "machine_reboot", Launch: func(pty.Command) (PTYProcess, error) { t.Fatal("recovery must not launch"); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := recovered.Snapshot(created.ID)
	if err != nil || snapshot.State != Exited || snapshot.Exit == nil || snapshot.Exit.Signal != "machine_reboot" || snapshot.LatestSequence < uint64(len("retained")) {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestManagerRecoveryClearsLegacyExitedHistory(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	state, err := store.Open(context.Background(), store.Config{Root: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := store.Session{ID: "ses_compact", Name: "compact", CWD: t.TempDir(), CommandPath: "/bin/sh", CommandArgs: []string{"-c", "exit"}, CommandEnv: []string{"PATH=/bin"}, Columns: 80, Rows: 24, State: "exited", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := state.CreateSession(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	for sequence, data := range [][]byte{[]byte("abc"), []byte("def"), []byte("ghi")} {
		if _, _, err := state.AppendOutput(context.Background(), record.ID, 1, uint64(sequence*3), data, 64); err != nil {
			t.Fatal(err)
		}
	}
	defer state.Close()
	manager, err := NewManager(ManagerConfig{Store: state, Launch: func(pty.Command) (PTYProcess, error) { t.Fatal("recovery must not launch"); return nil, nil }, HistoryBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(record.ID)
	if err != nil || snapshot.EarliestSequence != 9 || snapshot.LatestSequence != 9 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	attached, err := manager.Attach(record.ID, "att_compact", snapshot.EarliestSequence)
	if err != nil || len(attached.Replay.Events) != 0 {
		t.Fatalf("attached=%#v err=%v", attached, err)
	}
}

// Close must retain recovery across both history and lifecycle write failures.
func TestManagerCloseRecoversPersistenceFailures(t *testing.T) {
	for _, fault := range []string{"output", "lifecycle"} {
		t.Run(fault, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			injected := errors.New("injected output persistence failure")
			failed := make(chan struct{}, 1)
			state, err := store.Open(t.Context(), store.Config{Root: root, FailureHook: func(point string) error {
				if fault == "output" && point == "append_before_commit" {
					select {
					case failed <- struct{}{}:
					default:
					}
					return injected
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			workspace := t.TempDir()
			adapter, err := pty.NewAdapter(workspace)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := NewManager(ManagerConfig{Store: state, Launch: func(command pty.Command) (PTYProcess, error) { return adapter.Start(command) }, TerminationTimeout: time.Second, TerminationGrace: 10 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Shutdown(context.Background())
			created, err := manager.Create(t.Context(), CreateRequest{Name: "close-recovery", Command: shellCommand("/bin/sh", workspace, "printf ready; read line")})
			if err != nil {
				t.Fatal(err)
			}
			waitLatest(t, manager, created.ID, 5)
			var db *sql.DB
			if fault == "output" {
				select {
				case <-failed:
				case <-time.After(time.Second):
					t.Fatal("output failure not reached")
				}
			} else {
				db, err = sql.Open("sqlite", filepath.Join(root, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				_, err = db.Exec(`CREATE TRIGGER reject_close BEFORE UPDATE OF state ON sessions WHEN NEW.state IN ('closing','closed') BEGIN SELECT RAISE(ABORT,'injected lifecycle failure'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			closed, err := manager.Close(t.Context(), created.ID)
			if err == nil {
				t.Fatal("injected persistence failure was hidden")
			}
			if fault == "output" && !errors.Is(err, injected) {
				t.Fatal("output cause lost")
			}
			if closed.State != Closed || closed.Exit == nil || closed.Exit.ExitedAt.IsZero() {
				t.Fatalf("close did not reap process: state=%s", closed.State)
			}
			if fault == "lifecycle" {
				if _, err = db.Exec(`DROP TRIGGER reject_close`); err != nil {
					t.Fatal(err)
				}
			}
			closed, err = manager.CloseAtGeneration(t.Context(), created.ID, created.Generation)
			if err != nil || closed.State != Closed || closed.Generation != created.Generation {
				t.Fatalf("close retry state=%s generation=%d err=%v", closed.State, closed.Generation, err)
			}
			records, err := state.Sessions(t.Context())
			if err != nil || len(records) != 1 || records[0].State != string(Closed) || records[0].Generation != created.Generation {
				t.Fatalf("durable close not recovered: records=%d err=%v", len(records), err)
			}
			output, earliest, latest, err := state.Replay(t.Context(), created.ID, closed.LatestSequence, 0)
			if err != nil || len(output) != 0 || earliest != latest {
				t.Fatal("closed output not cleared")
			}
			if err = manager.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = state.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(t.Context(), store.Config{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			recovered, err := NewManager(ManagerConfig{Store: reopened, Launch: func(pty.Command) (PTYProcess, error) { t.Fatal("closed recovery launched process"); return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := recovered.Snapshot(created.ID)
			if err != nil || snapshot.State != Closed || snapshot.Generation != created.Generation {
				t.Fatal("reopen lost closed generation")
			}
		})
	}
}
