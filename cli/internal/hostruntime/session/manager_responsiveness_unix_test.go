//go:build darwin || linux

package session

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

type stalledSessionProcess struct {
	*capacityProcess
	resize    func()
	signal    func()
	terminate func()
}

func (p *stalledSessionProcess) Resize(pty.Dimensions) error {
	if p.resize != nil {
		p.resize()
	}
	return nil
}
func (p *stalledSessionProcess) Signal(pty.Signal) error {
	if p.signal != nil {
		p.signal()
	}
	return nil
}
func (p *stalledSessionProcess) Terminate(ctx context.Context, grace time.Duration) (pty.ExitResult, error) {
	if p.terminate != nil {
		p.terminate()
	}
	return p.capacityProcess.Terminate(ctx, grace)
}
func responsivenessManager(t *testing.T, state *store.Store, launch ContextLauncher) (*Manager, *stalledSessionProcess, Snapshot) {
	t.Helper()
	process := &stalledSessionProcess{capacityProcess: &capacityProcess{done: make(chan struct{}), exited: new(atomic.Int32)}}
	if launch == nil {
		var launched atomic.Bool
		launch = func(context.Context, pty.Command) (PTYProcess, error) {
			if !launched.Swap(true) {
				return process, nil
			}
			return &capacityProcess{done: make(chan struct{}), exited: new(atomic.Int32)}, nil
		}
	}
	manager, err := NewManager(ManagerConfig{Store: state, LaunchContext: launch})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(t.Context(), CreateRequest{Name: "responsive", Command: pty.Command{Path: "/bin/sh", CWD: t.TempDir(), Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.AttachParticipant(created.ID, Participant{AttachmentID: "owner", Role: "owner", ConnectedAt: time.Now()}, 0); err != nil {
		t.Fatal(err)
	}
	return manager, process, created
}
func assertListResponsive(t *testing.T, manager *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() { manager.List(); manager.ResourceCounts(); close(done) }()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("List/health blocked behind terminal I/O")
	}
}
func TestManagerListWhilePTYResizeStalls(t *testing.T) {
	manager, process, created := responsivenessManager(t, nil, nil)
	entered, secondEntered, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	process.resize = func() {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		} else {
			close(secondEntered)
		}
	}
	done := make(chan error, 2)
	started := 1
	defer func() {
		unblock()
		for range started {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}()
	go func() { done <- manager.Resize(created.ID, "owner", pty.Dimensions{Columns: 100, Rows: 30}) }()
	<-entered
	assertListResponsive(t, manager)
	started++
	go func() { done <- manager.Resize(created.ID, "owner", pty.Dimensions{Columns: 120, Rows: 40}) }()
	select {
	case <-secondEntered:
		t.Fatal("PTY resizes overlapped")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	for range started {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	started = 0
	snapshot, err := manager.Snapshot(created.ID)
	if err != nil || snapshot.Dimensions != (pty.Dimensions{Columns: 120, Rows: 40}) {
		t.Fatalf("resize order lost: %+v, %v", snapshot, err)
	}
}

func TestManagerListWhilePTYLifecycleStalls(t *testing.T) {
	for _, operation := range []string{"signal", "terminate", "create", "restart"} {
		t.Run(operation, func(t *testing.T) {
			manager, process, created := responsivenessManager(t, nil, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			stall := func() { close(entered); <-release }
			done := make(chan error, 1)
			switch operation {
			case "signal":
				process.signal = stall
			case "terminate":
				process.terminate = stall
			case "restart":
				if _, err := manager.Close(t.Context(), created.ID); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case "create":
				manager.config.LaunchContext = func(context.Context, pty.Command) (PTYProcess, error) {
					stall()
					return &capacityProcess{done: make(chan struct{}), exited: new(atomic.Int32)}, nil
				}
			}
			defer func() {
				close(release)
				if err := <-done; err != nil {
					t.Error(err)
				}
				process.terminate = nil
			}()
			go func() {
				var err error
				switch operation {
				case "signal":
					err = manager.Signal(created.ID, created.Generation, pty.Interrupt)
				case "terminate":
					_, err = manager.Close(t.Context(), created.ID)
				case "create":
					_, err = manager.Create(t.Context(), CreateRequest{Name: "second", Command: pty.Command{Dimensions: created.Dimensions}})
				case "restart":
					_, err = manager.Restart(created.ID)
				}
				done <- err
			}()
			<-entered
			assertListResponsive(t, manager)
		})
	}
}
func TestManagerListWhileStoreWriteStalls(t *testing.T) {
	for _, operation := range []string{"geometry", "create", "close", "clear", "delete", "restart"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			state, err := store.Open(t.Context(), store.Config{Root: filepath.Join(t.TempDir(), "state"), FailureHook: func(point string) error {
				if point == "append_before_commit" {
					once.Do(func() { close(entered) })
					<-release
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := state.Close(); err != nil {
					t.Error(err)
				}
			})
			manager, _, created := responsivenessManager(t, state, nil)
			if operation == "delete" || operation == "restart" {
				if _, err := manager.Close(t.Context(), created.ID); err != nil {
					t.Fatal(err)
				}
			}
			diskDone := make(chan error, 1)
			go func() { diskDone <- state.AppendOutputBatch(t.Context(), nil, 1024) }()
			<-entered
			done := make(chan error, 1)
			defer func() {
				close(release)
				if err := <-diskDone; err != nil {
					t.Error(err)
				}
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			go func() {
				var err error
				switch operation {
				case "geometry":
					err = manager.Resize(created.ID, "owner", pty.Dimensions{Columns: 100, Rows: 30})
				case "create":
					_, err = manager.Create(t.Context(), CreateRequest{Name: "second", Command: pty.Command{Dimensions: created.Dimensions}})
				case "close":
					_, err = manager.Close(t.Context(), created.ID)
				case "clear":
					_, err = manager.Clear(created.ID)
				case "delete":
					err = manager.Delete(created.ID)
				case "restart":
					_, err = manager.Restart(created.ID)
				}
				done <- err
			}()
			// Exercise observations throughout the blocked transaction, including after
			// the mutation goroutine has reached SQLite rather than just been scheduled.
			until := time.After(150 * time.Millisecond)
			for {
				select {
				case <-until:
					return
				default:
					assertListResponsive(t, manager)
				}
			}
		})
	}
}

// A resize that completes after close/restart belongs to the old process. It
// must neither delay the replacement nor overwrite its grid or replay history.
func TestManagerRestartWhileOldResizeStalls(t *testing.T) {
	manager, process, created := responsivenessManager(t, nil, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	process.resize = func() { close(entered); <-release }
	resized := make(chan error, 1)
	go func() { resized <- manager.Resize(created.ID, "owner", pty.Dimensions{Columns: 100, Rows: 30}) }()
	<-entered
	attached := make(chan error, 1)
	go func() {
		_, err := manager.AttachParticipant(created.ID, Participant{AttachmentID: "second", Role: "owner", ConnectedAt: time.Now(), Dimensions: pty.Dimensions{Columns: 120, Rows: 40}}, 0)
		attached <- err
	}()
	select {
	case err := <-attached:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		unblock()
		<-attached
		t.Fatal("attach blocked behind resize")
	}
	restarted := make(chan Snapshot, 1)
	go func() {
		if _, err := manager.Close(context.Background(), created.ID); err != nil {
			t.Error(err)
		}
		snapshot, err := manager.Restart(created.ID)
		if err != nil {
			t.Error(err)
		}
		restarted <- snapshot
	}()
	joined := false
	defer func() {
		unblock()
		if !joined {
			<-restarted
		}
	}()
	select {
	case snapshot := <-restarted:
		joined = true
		if snapshot.Generation != created.Generation+1 || snapshot.Dimensions != created.Dimensions {
			t.Fatalf("replacement = %+v", snapshot)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("restart blocked behind old resize")
	}
	// Join resize before checking the surviving generation's state.
	unblock()
	if err := <-resized; !errors.Is(err, ErrStaleGeneration) {
		t.Errorf("old resize error = %v, want stale generation", err)
	}
	until := time.After(100 * time.Millisecond)
	for {
		snapshot, err := manager.Snapshot(created.ID)
		if err != nil || snapshot.Dimensions != created.Dimensions || snapshot.EarliestSequence != snapshot.LatestSequence {
			t.Fatalf("stale resize changed replacement: %+v, %v", snapshot, err)
		}
		select {
		case <-until:
			return
		default:
		}
	}
}

type delayedOutputProcess struct {
	*capacityProcess
	entered   chan struct{}
	release   chan struct{}
	consumed  chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	read      bool
}

func (p *delayedOutputProcess) Read(buffer []byte) (int, error) {
	if !p.read {
		p.read = true
		close(p.entered)
		<-p.release
		return copy(buffer, "old output"), nil
	}
	close(p.consumed)
	return 0, io.EOF
}
func (p *delayedOutputProcess) CloseIO() error {
	if p.closed != nil {
		p.closeOnce.Do(func() { close(p.closed) })
	}
	return p.capacityProcess.CloseIO()
}
func TestManagerOldCaptureCannotWriteIntoReplacement(t *testing.T) {
	old := &delayedOutputProcess{capacityProcess: &capacityProcess{done: make(chan struct{}), exited: new(atomic.Int32)}, entered: make(chan struct{}), release: make(chan struct{}), consumed: make(chan struct{})}
	var launched atomic.Bool
	manager, _, created := responsivenessManager(t, nil, func(context.Context, pty.Command) (PTYProcess, error) {
		if !launched.Swap(true) {
			return old, nil
		}
		return &capacityProcess{done: make(chan struct{}), exited: new(atomic.Int32)}, nil
	})
	<-old.entered
	var once sync.Once
	unblock := func() { once.Do(func() { close(old.release) }) }
	defer unblock()
	if _, err := manager.Close(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Restart(created.ID); err != nil {
		t.Fatal(err)
	}
	unblock()
	<-old.consumed
	snapshot, err := manager.Snapshot(created.ID)
	if err != nil || snapshot.LatestSequence != 0 {
		t.Fatalf("old capture polluted replacement: %+v, %v", snapshot, err)
	}
}

func TestManagerOldCaptureCannotStopReplacementPersistence(t *testing.T) {
	state, err := store.Open(t.Context(), store.Config{Root: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	makeProcess := func() *delayedOutputProcess {
		return &delayedOutputProcess{capacityProcess: &capacityProcess{done: make(chan struct{}), exited: new(atomic.Int32)}, entered: make(chan struct{}), release: make(chan struct{}), consumed: make(chan struct{}), closed: make(chan struct{})}
	}
	old, next := makeProcess(), makeProcess()
	var launched atomic.Bool
	manager, _, created := responsivenessManager(t, state, func(context.Context, pty.Command) (PTYProcess, error) {
		if !launched.Swap(true) {
			return old, nil
		}
		return next, nil
	})
	<-old.entered
	var oldOnce, nextOnce sync.Once
	unblockOld := func() { oldOnce.Do(func() { close(old.release) }) }
	unblockNext := func() { nextOnce.Do(func() { close(next.release) }) }
	defer unblockOld()
	defer unblockNext()
	if _, err := manager.Close(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Restart(created.ID); err != nil {
		t.Fatal(err)
	}
	<-next.entered
	unblockOld()
	<-old.closed
	unblockNext()
	<-next.consumed
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, latest, err := state.OutputBounds(t.Context(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if latest == uint64(len("old output")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("old capture stopped the replacement's output persistence")
}
