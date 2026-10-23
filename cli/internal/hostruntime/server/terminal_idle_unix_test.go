//go:build darwin || linux

package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type countingSessions struct {
	session.Service
	snapshots atomic.Int64
}

func (c *countingSessions) Snapshot(sessionID string) (session.Snapshot, error) {
	c.snapshots.Add(1)
	return c.Service.Snapshot(sessionID)
}

func idleTestSessions(t *testing.T, script string) (*countingSessions, session.Snapshot) {
	t.Helper()
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Shutdown(context.Background()) })
	created, err := manager.Create(context.Background(), session.CreateRequest{Name: "idle", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", script}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	return &countingSessions{Service: manager}, created
}

// Each idle terminal stream checked the session every 100 ms with two snapshots: 1,000 idle viewers
// cost 20,000 snapshots a second on one host. An idle stream now backs off to one check every 2 s.
func TestIdleTerminalStreamsBackOffTheirSessionChecks(t *testing.T) {
	sessions, created := idleTestSessions(t, "exec sleep 60")
	const viewers = 50
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var wait sync.WaitGroup
	for i := range viewers {
		id := fmt.Sprintf("att_idle_%02d", i)
		if _, err := sessions.Attach(created.ID, id, 0); err != nil {
			t.Fatal(err)
		}
		stream := &terminalOutputStream{manager: sessions, sessionID: created.ID, attachmentID: id, checkpointSent: true}
		wait.Add(1)
		go func() {
			defer wait.Done()
			for ctx.Err() == nil {
				if frame, err := stream.Next(ctx); err == nil && frame.Release != nil {
					frame.Release()
				}
			}
		}()
	}
	wait.Wait()
	// Waits of 0.1, 0.2, 0.4, 0.8, 1.6 s fit in 3 s: about 6 snapshots per stream (the old loop took ~60).
	if got := sessions.snapshots.Load(); got > viewers*10 {
		t.Fatalf("%d session snapshots from %d idle streams in 3 s, want about %d", got, viewers, viewers*6)
	}
}

// A shell that exits after a long silence still ends its stream within the longest idle wait.
func TestIdleTerminalStreamStillEndsWhenTheSessionExits(t *testing.T) {
	sessions, created := idleTestSessions(t, "sleep 3")
	if _, err := sessions.Attach(created.ID, "att_exit", 0); err != nil {
		t.Fatal(err)
	}
	stream := &terminalOutputStream{manager: sessions, sessionID: created.ID, attachmentID: "att_exit", checkpointSent: true}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := time.Now()
	for {
		frame, err := stream.Next(ctx)
		if err == nil {
			if frame.Release != nil {
				frame.Release()
			}
			continue
		}
		var end *StreamEnd
		if !errors.As(err, &end) {
			t.Fatalf("stream ended with %v, want the terminal stream end", err)
		}
		break
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second+terminalIdleObservationMaximum+500*time.Millisecond {
		t.Fatalf("exit after 3 s of silence noticed after %v", elapsed)
	}
}
