//go:build darwin || linux

package session

import (
	"context"
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
		launch = func(context.Context, pty.Command) (PTYProcess, error) { return process, nil }
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
	entered, release := make(chan struct{}), make(chan struct{})
	process.resize = func() { close(entered); <-release }
	done := make(chan error, 1)
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	go func() { done <- manager.Resize(created.ID, "owner", pty.Dimensions{Columns: 100, Rows: 30}) }()
	<-entered
	assertListResponsive(t, manager)
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
