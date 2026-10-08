//go:build darwin || linux

package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalTerminalDirectorySurvivesDetachAndReconnect(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer sessions.Shutdown(ctx)
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := process.NewShellLauncher(shell, []string{"PATH=/usr/bin:/bin", "HOME=" + root, "TERM=xterm"}, sessions)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherConfig{Sessions: sessions, SessionLauncher: launcher, Health: health.New("test", []string{"terminal.v1"}, nil), WorkspaceRoot: root, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"action": "create", "session_id": "ses_local_pwd", "name": "local-pwd", "cwd": cwd, "columns": 80, "rows": 24})
	outcome := dispatcher.Handle(ctx, Authorization{ClientID: "local-client", UserID: "local-owner", SessionID: "ses_local_pwd"}, "terminal.v1", payload)
	if outcome.ErrorCode != "" {
		t.Fatalf("create outcome=%+v", outcome)
	}
	attached, err := sessions.Attach("ses_local_pwd", "first", 0)
	if err != nil {
		t.Fatal(err)
	}
	attached.Replay.Release()
	if err := sessions.WriteStream("ses_local_pwd", "first", attached.Snapshot.Generation, []byte("printf 'PB_PWD='; pwd -P\n")); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	want, _ := filepath.EvalSymlinks(cwd)
	for !strings.Contains(output.String(), "PB_PWD="+want) {
		event, err := sessions.WaitNext(ctx, "ses_local_pwd", "first")
		if err != nil {
			t.Fatal(err)
		}
		output.Write(event.Data)
		event.Release()
	}
	if err := sessions.Detach("ses_local_pwd", "first"); err != nil {
		t.Fatal(err)
	}
	reattached, err := sessions.Attach("ses_local_pwd", "second", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reattached.Replay.Release()
	var replay strings.Builder
	for _, event := range reattached.Replay.Events {
		replay.Write(event.Data)
	}
	if reattached.Snapshot.Generation != attached.Snapshot.Generation || reattached.Snapshot.CWD != cwd || !strings.Contains(replay.String(), "PB_PWD="+want) {
		t.Fatalf("reconnect snapshot=%+v replay=%q", reattached.Snapshot, replay.String())
	}
	if _, err := sessions.Close(ctx, "ses_local_pwd"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Delete("ses_local_pwd"); err != nil {
		t.Fatal(err)
	}
	if len(sessions.List()) != 0 {
		t.Fatal("session cleanup failed")
	}
}
