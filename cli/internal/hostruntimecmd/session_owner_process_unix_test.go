//go:build linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// Exercise the packaged private entry point, including actual process custody
// and reopening its original durable store. Unit bridge tests do not launch pb.
func TestPackagedSessionOwnerSurvivesClientAndReopensHistory(t *testing.T) {
	binary := os.Getenv("PAPERBOAT_OWNER_TEST_BINARY")
	if binary == "" || os.Geteuid() != 0 {
		t.Skip("requires built pb and privileged descriptor publication")
	}
	root, err := os.MkdirTemp("/var/lib", "paperboat-owner-process-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "pb")
	if err := os.WriteFile(pin, body, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(pin, "dev-owner-process-test", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: source.SHA256[:32], Instance: "process-test", Source: source, Executable: pin, RuntimeRoot: filepath.Join(root, "state"), InitialStore: true, Endpoint: filepath.Join(root, "ipc", "owner.sock"), Workspace: root, Shell: "/bin/sh"}
	for _, path := range []string{d.PrivateRoot(), filepath.Dir(d.Endpoint), filepath.Join(root, "catalog")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	capability := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32))
	if err := os.WriteFile(d.TokenPath(), []byte(capability), 0600); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "catalog", d.Generation+".json"), encoded, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := func(descriptor sessionowner.Descriptor) func() {
		command := exec.Command(pin, "daemon", "__runtime-session-owner", "--generation", descriptor.Generation)
		command.Env = []string{"PATH=/bin:/usr/bin", "PAPERBOAT_SESSION_OWNER_CATALOG=" + filepath.Join(root, "catalog")}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("private owner exit: %v", err)
				}
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Error("private owner did not stop")
			}
		}
		t.Cleanup(stop)
		return stop
	}
	stop := start(d)
	client, err := sessionowner.WaitReady(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	created, err := remote.Create(ctx, session.CreateRequest{ID: "preserved-shell", Name: "preserved-shell", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "printf 'preserved-history\\n'; read line"}, CWD: root, Env: []string{"PATH=/bin:/usr/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	client, err = sessionowner.Connect(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, _ = client.Sessions()
	if snapshot, err := remote.Snapshot(created.ID); err != nil || snapshot.State != session.Running {
		t.Fatalf("shell lost with client: %v", err)
	}
	if err := client.Retire(ctx); !errors.Is(err, session.ErrUpdateBusy) {
		t.Fatalf("live shell retirement: %v", err)
	}
	if _, err := remote.AttachLive(created.ID, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Write(created.ID, session.InputKey{ClientID: "test", AttachmentID: "writer", Generation: created.Generation, InputID: "finish"}, []byte("finish\n")); err != nil {
		t.Fatal(err)
	}
	for {
		snapshot, err := remote.Snapshot(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == session.Exited {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := client.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	client.Close()
	stop()
	stop = start(d)
	defer stop()
	client, err = sessionowner.WaitReady(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, _ = client.Sessions()
	attached, err := remote.Attach(created.ID, "history", 0)
	if err != nil || attached.Snapshot.State != session.Exited {
		t.Fatalf("archived shell lost: %v", err)
	}
	var replay []byte
	for _, event := range attached.Replay.Events {
		replay = append(replay, event.Data...)
	}
	if !bytes.Contains(replay, []byte("preserved-history")) {
		t.Fatal("original store lost closed terminal output")
	}
	if _, err := os.Stat(filepath.Join(d.RuntimeRoot, "state.db")); err != nil {
		t.Fatal("original durable store was not used")
	}
	// The production wrapper must observe protected catalogue removal rather
	// than retaining an unreachable generation in its routing index.
	empty := d
	empty.Generation = strings.Repeat("c", 32)
	if empty.Generation == d.Generation {
		empty.Generation = strings.Repeat("d", 32)
	}
	empty.InitialStore = false
	empty.Endpoint = filepath.Join(root, "ipc", "empty.sock")
	if err := os.MkdirAll(empty.PrivateRoot(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty.TokenPath(), []byte(capability), 0600); err != nil {
		t.Fatal(err)
	}
	emptyBody, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(root, "catalog", empty.Generation+".json")
	if err := os.WriteFile(emptyPath, emptyBody, 0644); err != nil {
		t.Fatal(err)
	}
	stopEmpty := start(empty)
	emptyClient, err := sessionowner.WaitReady(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	defer emptyClient.Close()
	catalog := sessionowner.Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: "/", User: "root", Group: "root", Maximum: 21}
	connections, err := sessionowner.NewConnections(ctx, catalog, d, func(context.Context, string) error { return session.ErrOwnerUnavailable }, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer connections.Close()
	routed, err := connections.Service()
	if err != nil {
		t.Fatal(err)
	}
	if got := routed.List(); len(got) != 1 || got[0].ID != created.ID || routed.LastError() != nil {
		t.Fatal("protected initial catalogue did not expose archived terminal")
	}
	if err := emptyClient.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	stopEmpty()
	if _, err := os.Stat(empty.PrivateRoot()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty isolated store retained: %v", err)
	}
	if err := os.Remove(emptyPath); err != nil {
		t.Fatal(err)
	}
	if got := routed.List(); len(got) != 1 || got[0].ID != created.ID || routed.LastError() != nil {
		t.Fatal("production catalogue removal broke archived list")
	}
	if err := remote.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	identity, err := client.OwnerIdentity(ctx)
	if err != nil || !identity.Retired || identity.RetainedSessions != 0 {
		t.Fatalf("empty retirement not confirmed: %v", err)
	}
	stop()
	if _, err := os.Stat(d.PrivateRoot()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty private owner retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.RuntimeRoot, "state.db")); err != nil {
		t.Fatal("shared original database removed with empty terminal owner")
	}
}
