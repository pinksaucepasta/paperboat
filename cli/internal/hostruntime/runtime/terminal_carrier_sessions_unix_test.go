//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

func TestTerminalCarrierContinuesPTYWithoutLaunchingOrRestarting(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(c pty.Command) (session.PTYProcess, error) { return adapter.Start(c) }, MaxSessions: 4, MaxAttachments: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		manager.Shutdown(ctx)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request := session.CreateRequest{ID: "ses_continue", Name: "continue", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "stty -echo; exec cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}}
	before, err := manager.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	carrier := continuingSessions{Service: manager}
	if _, err := carrier.Create(ctx, request); !errors.Is(err, session.ErrSessionExists) {
		t.Fatal("carrier did not preserve existing-session create-or-get", err)
	}
	request.ID, request.Name = "ses_missing", "missing"
	if _, err := carrier.Create(ctx, request); err == nil || len(manager.List()) != 1 {
		t.Fatal("delegated carrier launched a new PTY outside worker launch policy")
	}
	if _, err := carrier.RestartAtGenerationContext(ctx, before.ID, before.Generation); !errors.Is(err, session.ErrInvalidTransition) {
		t.Fatal("delegated carrier restarted the PTY outside worker launch policy", err)
	}
	attached, err := carrier.Attach(before.ID, "att_continuing", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := carrier.Write(before.ID, session.InputKey{ClientID: "client", AttachmentID: "att_continuing", Generation: before.Generation, InputSequence: 1}, []byte("continue-owner\n")); err != nil {
		t.Fatal(err)
	}
	var output []byte
	for !bytes.Contains(output, []byte("continue-owner")) {
		event, err := carrier.WaitNextAttachment(ctx, before.ID, "att_continuing", 0, attached.AttachmentLease)
		if err != nil {
			t.Fatal("original PTY did not continue input/output", err)
		}
		output = append(output, event.Data...)
		event.Release()
	}
	after, err := carrier.Snapshot(before.ID)
	if err != nil || after.Generation != before.Generation {
		t.Fatal("carrier replaced the original shell", err)
	}
}
