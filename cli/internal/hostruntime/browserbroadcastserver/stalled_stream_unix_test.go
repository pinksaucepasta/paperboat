//go:build darwin || linux

package browserbroadcastserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// A publisher stream nobody reads any more (an edge that stopped reading) must not block a viewer
// leaving: Leave used to wait on the publication lock the stalled write held (Lane G CI deadlock).
func TestAStalledPublisherStreamDoesNotBlockLeave(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1, MaxAttachments: 8, HistoryBytes: 1 << 20, AttachmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	}()
	// The shell prints continuously, so the publisher always has a record to write.
	created, err := manager.Create(context.Background(), session.CreateRequest{Name: "stalled", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "while :; do echo output; done"}, CWD: root, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(manager, func(context.Context) (ed25519.PrivateKey, error) { return private, nil })
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe() // never read: the first write stalls
	defer reader.Close()
	r.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return writer, nil })
	defer r.SetPublisher(nil)
	for _, id := range []string{"browser_a", "browser_b"} {
		role := "interactive"
		if id == "browser_a" {
			role = "owner"
		}
		if _, err := manager.AttachLiveParticipantAtGeneration(created.ID, session.Participant{AttachmentID: id, AccountID: "account_" + id, ClientID: id, Role: role, ConnectedAt: time.Now(), Browser: true}, created.Generation); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Join(context.Background(), created.ID, created.Generation, id); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // the publisher is now blocked writing to the unread stream
	left := make(chan struct{})
	go func() {
		r.Leave(created.ID, "browser_b")
		r.Leave(created.ID, "browser_a")
		close(left)
	}()
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("Leave blocked behind a stalled publisher stream")
	}
}
