package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type joinTestConn struct{ tunnel.Conn }

// Lane N, F3-06: the join printed "Shared terminal … view only" before connecting, so a join stuck in
// the native dial looked attached for 90 s. The banner may only follow a connected dial.
func TestSharedJoinAnnouncesTheTerminalOnlyAfterItConnects(t *testing.T) {
	info := resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{}}
	info.Terminal.Auth.Scopes = []string{"terminal:view"}
	var out bytes.Buffer
	failure := errors.New("native dial failed")
	if _, err := dialSharedTerminal(context.Background(), &out, func(context.Context, resolver.ConnectInfo) (tunnel.Conn, error) {
		return nil, failure
	}, info, "terminal_1"); !errors.Is(err, failure) {
		t.Fatalf("dial error = %v", err)
	}
	if strings.Contains(out.String(), "Shared terminal terminal_1") || !strings.Contains(out.String(), "Connecting to shared terminal terminal_1") {
		t.Fatalf("failed join output = %q", out.String())
	}
	out.Reset()
	conn, err := dialSharedTerminal(context.Background(), &out, func(context.Context, resolver.ConnectInfo) (tunnel.Conn, error) {
		return joinTestConn{}, nil
	}, info, "terminal_1")
	if err != nil || conn == nil || !strings.Contains(out.String(), "Shared terminal terminal_1 · view only") {
		t.Fatalf("connected join conn=%v err=%v output=%q", conn, err, out.String())
	}
}

// F3-06 (user decision 2026-10-23): an owner's upgrade to interactive reaches a joined teammate
// within seconds, without a rejoin. The watcher redials only when the role differs.
func TestWatchSharedRoleRedialsWhenTheRoleChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var serverViewer, joinedViewer atomic.Bool
	serverViewer.Store(true)
	joinedViewer.Store(true)
	redials := make(chan struct{}, 4)
	go watchSharedRole(ctx, 10*time.Millisecond, func(context.Context) (bool, error) { return serverViewer.Load(), nil }, joinedViewer.Load, func() {
		joinedViewer.Store(serverViewer.Load())
		redials <- struct{}{}
	})
	select {
	case <-redials:
		t.Fatal("redialed while the role was unchanged")
	case <-time.After(100 * time.Millisecond):
	}
	serverViewer.Store(false)
	select {
	case <-redials:
	case <-time.After(2 * time.Second):
		t.Fatal("an upgrade to interactive did not redial")
	}
	select {
	case <-redials:
		t.Fatal("redialed again after the role matched")
	case <-time.After(100 * time.Millisecond):
	}
}

type recordingSessionCloser struct {
	machineID, sessionID string
	err                  error
}

func (r *recordingSessionCloser) CloseUserMachineTerminalSession(_ context.Context, machineID, sessionID string) (api.TerminalSessionCloseResult, error) {
	r.machineID, r.sessionID = machineID, sessionID
	return api.TerminalSessionCloseResult{}, r.err
}

// F3-05: a terminal the machine refused (for example at its ceiling) after the server created
// its row must not stay live on the server; connect closes the session it created.
func TestCloseAbandonedSessionClosesTheCreatedSession(t *testing.T) {
	closer := &recordingSessionCloser{}
	if err := closeAbandonedSession(closer, "machine_1", "terminal_1"); err != nil || closer.machineID != "machine_1" || closer.sessionID != "terminal_1" {
		t.Fatalf("close machine=%q session=%q err=%v", closer.machineID, closer.sessionID, err)
	}
	closer.err = errors.New("server unreachable")
	if err := closeAbandonedSession(closer, "machine_1", "terminal_1"); err == nil || !strings.Contains(err.Error(), "did not attach") {
		t.Fatalf("a failed cleanup must name the step: %v", err)
	}
}
