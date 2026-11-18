//go:build darwin || linux

package execprocess

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Output bytes cross the owner, hostd and worker hops as a length-prefixed frame: the wire holds the bytes once (plus
// a small header), not base64 inside a JSON string, and they arrive intact through the reader with its per-event ack.
func TestReaderOutputCrossesBothHopsAsRawFrames(t *testing.T) {
	root := t.TempDir()
	owner, err := New(Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 4, MaximumOperations: 16, ReplayBytes: 4 << 20, ChunkBytes: 128 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = owner.Shutdown(ctx)
	})
	inner := &RemoteServer{Manager: owner, MaximumReaders: 8}
	forwarded, err := NewRemoteManager(inner.Call)
	if err != nil {
		t.Fatal(err)
	}
	hostd := &RemoteServer{Manager: forwarded, MaximumReaders: 8}
	var largest int
	client, err := NewRemoteManager(func(ctx context.Context, op string, body []byte) ([]byte, error) {
		out, err := hostd.Call(ctx, op, body)
		if op == "reader_next" {
			largest = max(largest, len(out))
		}
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	const total = 1 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	execution, _, err := client.Start(ctx, Request{OperationID: "operation_frame", Argv: []string{"/bin/sh", "-c", "head -c 1048576 /dev/zero | tr '\\0' 'x'"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReader(1)
	if err != nil {
		t.Fatal(err)
	}
	var output []byte
	for {
		event, release, err := reader.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if event.Stream == "stdout" {
			output = append(output, event.Data...)
		}
		done := event.Result != nil
		release()
		if done {
			break
		}
	}
	if !bytes.Equal(output, bytes.Repeat([]byte("x"), total)) {
		t.Fatalf("got %d bytes, want %d intact", len(output), total)
	}
	if limit := 128<<10 + 4<<10; largest > limit {
		t.Fatalf("largest reader_next answer is %d bytes: the data was re-encoded (limit %d for a 128 KiB chunk)", largest, limit)
	}
}
