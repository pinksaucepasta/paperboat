//go:build linux

package session

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func TestManagerRetireRefusesLivePTYThenFencesAdmissions(t *testing.T) {
	workspace := t.TempDir()
	adapter, err := pty.NewAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		Launch:             func(command pty.Command) (PTYProcess, error) { return adapter.Start(command) },
		TerminationTimeout: 3 * time.Second,
		TerminationGrace:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })

	created, err := manager.Create(context.Background(), CreateRequest{
		Name:    "retire-live",
		Command: shellCommand("/bin/sh", workspace, "read line; printf retired; exit 0"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Retire(context.Background()); !errors.Is(err, ErrUpdateBusy) {
		t.Fatalf("retire with live PTY: got %v, want ErrUpdateBusy", err)
	}
	snapshot, err := manager.Snapshot(created.ID)
	if err != nil || snapshot.State != Running || snapshot.Generation != created.Generation {
		t.Fatalf("live session lost after rejected retire: snapshot=%#v err=%v", snapshot, err)
	}
	if _, err := manager.AttachLive(created.ID, "att_retire"); err != nil {
		t.Fatalf("manager did not retain the live session after rejected retire: %v", err)
	}
	decision, err := manager.Write(created.ID, InputKey{ClientID: "cli", AttachmentID: "att_retire", Generation: created.Generation, InputID: "finish"}, []byte("finish\n"))
	if err != nil || decision.Status != InputAccepted {
		t.Fatalf("session could not continue after rejected retire: decision=%#v err=%v", decision, err)
	}
	waitState(t, manager, created.ID, Exited)

	if err := manager.Retire(context.Background()); err != nil {
		t.Fatalf("retire after process exit: %v", err)
	}
	if _, err := manager.Create(context.Background(), CreateRequest{
		Name:    "after-retire",
		Command: shellCommand("/bin/sh", workspace, "read line"),
	}); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("create after retire: got %v, want ErrManagerStopped", err)
	}
	if _, err := manager.Restart(created.ID); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("restart after retire: got %v, want ErrManagerStopped", err)
	}
	if err := manager.Retire(context.Background()); err != nil {
		t.Fatalf("repeat retire: %v", err)
	}
}

func TestManagerRetireRetriesPersistenceAndRestoresStoppedHistory(t *testing.T) {
	workspace := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "state")
	var appendFailures atomic.Int32
	var blockFlush atomic.Bool
	enteredFlush := make(chan struct{}, 1)
	releaseFlush := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFlush) }) }
	t.Cleanup(release)
	injected := errors.New("temporary output persistence failure")
	state, err := store.Open(context.Background(), store.Config{
		Root: stateRoot,
		FailureHook: func(point string) error {
			if point != "append_before_commit" {
				return nil
			}
			if appendFailures.Add(1) == 1 {
				return injected
			}
			if blockFlush.CompareAndSwap(true, false) {
				enteredFlush <- struct{}{}
				<-releaseFlush
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	adapter, err := pty.NewAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	newManager := func(config ManagerConfig) *Manager {
		config.Store = state
		config.Launch = func(command pty.Command) (PTYProcess, error) { return adapter.Start(command) }
		config.HistoryBytes = 1 << 20
		config.AttachmentBytes = 1 << 20
		config.TerminationTimeout = 3 * time.Second
		config.TerminationGrace = 100 * time.Millisecond
		manager, err := NewManager(config)
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	manager := newManager(ManagerConfig{PreserveStoppedHistory: true})
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	created, err := manager.Create(context.Background(), CreateRequest{
		ID:      "terminal_00000000-0000-4000-8000-000000000037",
		Name:    "archived",
		Command: shellCommand("/bin/sh", workspace, "printf archive-prefix; read line; printf archive-suffix; exit 7"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AttachLive(created.ID, "att_archive"); err != nil {
		t.Fatal(err)
	}
	key := InputKey{ClientID: "cli", AttachmentID: "att_archive", Generation: created.Generation, InputID: "archive-input"}
	if decision, err := manager.Write(created.ID, key, []byte("finish\n")); err != nil || decision.Status != InputAccepted {
		t.Fatalf("input=%#v err=%v", decision, err)
	}
	waitLatest(t, manager, created.ID, uint64(len("archive-prefix")+len("archive-suffix")))
	exited := waitState(t, manager, created.ID, Exited)
	if exited.Exit == nil || exited.Exit.Code != 7 {
		t.Fatalf("exit metadata was lost: %#v", exited)
	}
	if appendFailures.Load() < 1 {
		t.Fatal("the test did not exercise a failed output persistence attempt")
	}
	before, err := state.Sessions(context.Background())
	if err != nil || len(before) != 1 || before[0].ID != created.ID {
		t.Fatalf("archive record before retire=%#v err=%v", before, err)
	}

	blockFlush.Store(true)
	retireCtx, cancelRetire := context.WithTimeout(context.Background(), time.Second)
	retireDone := make(chan error, 1)
	go func() { retireDone <- manager.Retire(retireCtx) }()
	select {
	case <-enteredFlush:
	case <-time.After(time.Second):
		cancelRetire()
		release()
		t.Fatal("retire retry did not reach the blocked output flush")
	}
	defer release()
	assertListResponsive(t, manager)
	if err := <-retireDone; !errors.Is(err, context.DeadlineExceeded) {
		cancelRetire()
		release()
		t.Fatalf("retire did not honor its deadline: %v", err)
	}
	cancelRetire()
	if _, err := manager.Snapshot(created.ID); err != nil {
		t.Fatalf("session ownership was lost after canceled retire: %v", err)
	}
	if _, err := manager.Create(context.Background(), CreateRequest{Name: "blocked-retry", Command: shellCommand("/bin/sh", workspace, "read line")}); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("failed retire did not keep admissions fenced: %v", err)
	}
	release()
	if err := manager.Retire(context.Background()); err != nil {
		t.Fatalf("retire retry: %v", err)
	}

	after, err := state.Sessions(context.Background())
	if err != nil || len(after) != 1 || after[0].ID != before[0].ID || after[0].Name != before[0].Name || after[0].State != before[0].State || after[0].Generation != before[0].Generation || after[0].ExitCode == nil || *after[0].ExitCode != 7 {
		t.Fatalf("retire changed stopped session metadata: before=%#v after=%#v err=%v", before, after, err)
	}
	stored, earliest, latest, err := state.Replay(context.Background(), created.ID, 0, 0)
	if err != nil {
		t.Fatalf("archived output replay: %v", err)
	}
	var output []byte
	for _, event := range stored {
		output = append(output, event.Data...)
	}
	if !bytes.Contains(output, []byte("archive-prefix")) || !bytes.Contains(output, []byte("archive-suffix")) || earliest >= latest {
		t.Fatalf("retire did not flush the complete replay tail: output=%q bounds=[%d,%d)", output, earliest, latest)
	}

	restored := newManager(ManagerConfig{PreserveStoppedHistory: true})
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	snapshot, err := restored.Snapshot(created.ID)
	if err != nil || snapshot.ID != created.ID || snapshot.Name != created.Name || snapshot.State != Exited || snapshot.Generation != created.Generation || snapshot.Exit == nil || snapshot.Exit.Code != 7 || snapshot.EarliestSequence != earliest || snapshot.LatestSequence != latest {
		t.Fatalf("archived session did not restore: snapshot=%#v err=%v", snapshot, err)
	}
	attached, err := restored.Attach(created.ID, "att_restore", 0)
	if err != nil {
		t.Fatal(err)
	}
	var replay []byte
	for _, event := range attached.Replay.Events {
		replay = append(replay, event.Data...)
	}
	if !bytes.Contains(replay, []byte("archive-prefix")) || !bytes.Contains(replay, []byte("archive-suffix")) {
		t.Fatalf("restored replay missing archived output: %q", replay)
	}
	if decision, err := restored.QueryInput(created.ID, key); err != nil || decision.Status != InputAccepted {
		t.Fatalf("restored input decision=%#v err=%v", decision, err)
	}
	restarted, err := restored.Restart(created.ID)
	if err != nil || restarted.ID != created.ID || restarted.Generation != created.Generation+1 || restarted.State != Running {
		t.Fatalf("restart from archived history: snapshot=%#v err=%v", restarted, err)
	}
	if _, err := restored.AttachLive(created.ID, "att_interrupted"); err != nil {
		t.Fatalf("attach restarted process: %v", err)
	}
	interruptedKey := InputKey{ClientID: "cli", AttachmentID: "att_interrupted", Generation: restarted.Generation, InputID: "interrupted-input"}
	if decision, err := restored.Write(created.ID, interruptedKey, []byte("finish\n")); err != nil || decision.Status != InputAccepted {
		t.Fatalf("finish restarted process: decision=%#v err=%v", decision, err)
	}
	waitState(t, restored, created.ID, Exited)
	if err := restored.Retire(context.Background()); err != nil {
		t.Fatalf("retire restarted process before crash simulation: %v", err)
	}
	interruptedRecords, err := state.Sessions(context.Background())
	if err != nil || len(interruptedRecords) != 1 || interruptedRecords[0].State != string(Exited) {
		t.Fatalf("stopped record before crash simulation=%#v err=%v", interruptedRecords, err)
	}
	interruptedRecord := interruptedRecords[0]
	interruptedTail, tailStart, tailEnd, err := state.Replay(context.Background(), created.ID, interruptedRecord.EarliestSequence, 0)
	if err != nil || len(interruptedTail) == 0 || tailEnd <= tailStart {
		t.Fatalf("stopped output was not durable before crash simulation: events=%#v bounds=[%d,%d) err=%v", interruptedTail, tailStart, tailEnd, err)
	}
	interruptedRecord.State = string(Running)
	interruptedRecord.ExitCode, interruptedRecord.ExitSignal, interruptedRecord.ExitedAt = nil, "", nil
	if err := state.UpdateSession(context.Background(), created.ID, string(Exited), interruptedRecord); err != nil {
		t.Fatalf("simulate persisted running state: %v", err)
	}
	interrupted := newManager(ManagerConfig{PreserveStoppedHistory: true})
	t.Cleanup(func() { _ = interrupted.Shutdown(context.Background()) })
	recovered, err := interrupted.Snapshot(created.ID)
	if err != nil || recovered.State != Exited || recovered.EarliestSequence != recovered.LatestSequence || recovered.Exit == nil || recovered.Exit.Signal != "runtime_restart" {
		t.Fatalf("interrupted running record did not use crash recovery: snapshot=%#v err=%v", recovered, err)
	}
	recoveredOutput, earliest, latest, err := state.Replay(context.Background(), created.ID, recovered.EarliestSequence, 0)
	if err != nil || len(recoveredOutput) != 0 || earliest != latest {
		t.Fatalf("interrupted output was preserved as archived history: events=%#v bounds=[%d,%d) err=%v", recoveredOutput, earliest, latest, err)
	}
}
