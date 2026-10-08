//go:build darwin || linux

package execprocess

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRemoteReaderLostAcknowledgementBoundsAndEpochCleanupPreserveExec(t *testing.T) {
	root := t.TempDir()
	owner := testManager(t, root, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer owner.Shutdown(context.Background())
	server := &RemoteServer{Manager: owner, MaximumReaders: 1}
	defer server.ResetReaders()
	lost := false
	remote, _ := NewRemoteManager(func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		result, err := server.Call(ctx, op, body)
		if op == "reader_ack" && !lost && err == nil {
			lost = true
			return nil, errors.New("lost acknowledgement response")
		}
		return result, err
	})
	execution, _, err := remote.Start(ctx, Request{OperationID: "preserved_remote_exec", Argv: []string{"/bin/sh", "-c", "printf ready; read item; printf reply; read finish"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReader(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = execution.OpenReader(1); !errors.Is(err, ErrCapacity) {
		t.Fatal("reader bound", err)
	}
	var output string
	var from uint64
	for !strings.Contains(output, "ready") {
		event, release, err := reader.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		output += string(event.Data)
		from = event.Sequence + 1
		release()
	}
	if !lost {
		t.Fatal("lost response path not exercised")
	}
	server.mu.Lock()
	retained := server.readers[reader.(*remoteReader).id]
	server.mu.Unlock()
	pending := make(chan error, 1)
	go func() {
		_, release, err := reader.Next(ctx)
		if release != nil {
			release()
		}
		pending <- err
	}()
	// Establish that Next holds the retained reader lock in its long poll.
	for retained.mu.TryLock() {
		retained.mu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("long poll did not begin")
		case <-time.After(time.Millisecond):
		}
	}
	// Reset must cancel a long poll before taking the reader lock it holds.
	if err := server.ResetReaders(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-pending:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("reader cleanup blocked")
	}
	fresh, err := remote.Get("preserved_remote_exec")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fresh.Write([]byte("continue\n")); err != nil {
		t.Fatal(err)
	}
	replay, err := fresh.OpenReader(from)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	output = ""
	for !strings.Contains(output, "reply") {
		event, release, err := replay.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		output += string(event.Data)
		release()
	}
	if err := fresh.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
}
