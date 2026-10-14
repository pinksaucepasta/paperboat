//go:build darwin || linux

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestHostWorkloadCountsFollowRealSessionLifecycle(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1, MaxAttachments: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := sessions.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	transfers := &filetransfer.Service{}
	if counts := hostWorkloadCounts(sessions, transfers); counts != (HostWorkloadCounts{}) {
		t.Fatalf("initial counts=%+v", counts)
	}
	_, err = sessions.Create(context.Background(), session.CreateRequest{ID: "diagnostic_session", Name: "diagnostic", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "while IFS= read -r line; do :; done"}, CWD: root, Env: []string{"PATH=/usr/bin:/bin"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	if counts := hostWorkloadCounts(sessions, transfers); counts.Sessions != 1 || counts.Processes != 1 || counts.Attachments != 0 || counts.Uploads != 0 {
		t.Fatalf("live counts=%+v", counts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := sessions.Close(ctx, "diagnostic_session"); err != nil {
		t.Fatal(err)
	}
	if counts := hostWorkloadCounts(sessions, transfers); counts.Sessions != 1 || counts.Processes != 0 || counts.Attachments != 0 || counts.Uploads != 0 {
		t.Fatalf("closed counts=%+v", counts)
	}
}
