//go:build linux || darwin

package workloadbridge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// The actual private wire must authenticate readiness, refuse exec access,
// leave a live shell usable after retirement refusal, and fence it after close.
func TestIndependentOwnerRetirementOverPrivateWire(t *testing.T) {
	root := shorttemp.Dir(t)
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(c pty.Command) (session.PTYProcess, error) { return adapter.Start(c) }, TerminationGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recorder := diagnostics.NewMemoryRecorder()
	ctx = diagnostics.WithRecorder(ctx, recorder)
	identity := OwnerIdentity{Schema: "paperboat.session-owner/v1", Generation: strings.Repeat("a", 32), ExecutableSHA256: strings.Repeat("b", 64)}
	token := make([]byte, 32)
	token[0] = 17
	server := &Server{Endpoint: filepath.Join(root, "ipc", "owner.sock"), Token: token, Sessions: manager, SessionOnly: true, Identity: &identity, Retire: manager.Retire, MaximumConcurrent: 8, Ready: make(chan error, 1), Fence: func() hostdproto.Status {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: identity.Generation, Epoch: 1}
	}, AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil }}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client, err := NewClient(server.Endpoint, token, identity.Generation, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	actual, err := client.OwnerIdentity(ctx)
	if err != nil || actual != identity {
		t.Fatalf("identity mismatch: %v", err)
	}
	wrong := append([]byte(nil), token...)
	wrong[0]++
	untrusted, _ := NewClient(server.Endpoint, wrong, identity.Generation, 1)
	defer untrusted.Close()
	if _, err := untrusted.OwnerIdentity(ctx); !errors.Is(err, session.ErrOwnerUnavailable) {
		t.Fatalf("unauthenticated readiness: %v", err)
	}
	if _, err := client.Call(ctx, "exec", "list", json.RawMessage(`{}`)); !errors.Is(err, session.ErrOwnerUnavailable) {
		t.Fatalf("exec admitted: %v", err)
	}
	remote, _ := client.Sessions()
	created, err := remote.Create(ctx, session.CreateRequest{ID: "retirement-shell", Name: "retirement", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "while read line; do printf 'reply:%s\\n' \"$line\"; done"}, Env: []string{"PATH=/bin:/usr/bin", "TERM=xterm"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Attach(created.ID, "attachment", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := remote.AttachmentStatus(created.ID, "attachment"); err != nil {
		t.Fatal(err)
	}
	var attached []diagnostics.Event
	for _, event := range recorder.Recent() {
		if event.Code == "owner_attachment_ready" {
			attached = append(attached, event)
		}
	}
	if len(attached) != 1 || attached[0].Fields["generation"] != identity.Generation {
		t.Fatalf("owner attachment phase records=%+v", attached)
	}
	if err := client.Retire(ctx); !errors.Is(err, session.ErrUpdateBusy) {
		t.Fatalf("live retirement: %v", err)
	}
	if snapshot, err := remote.Snapshot(created.ID); err != nil || snapshot.State != session.Running {
		t.Fatalf("shell lost after refusal: %v", err)
	}
	if _, err := remote.Close(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Restart(created.ID); !errors.Is(err, session.ErrManagerStopped) {
		t.Fatalf("retired owner accepted restart: %v", err)
	}
	if err := client.Retire(ctx); err != nil {
		t.Fatalf("retirement retry: %v", err)
	}
	if _, err := client.OwnerIdentity(ctx); err != nil {
		t.Fatalf("retirement confirmation unavailable: %v", err)
	}
}
