//go:build darwin || linux

package execprocess

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
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

// The header cap equals the bridge's body cap, so it can never refuse a legitimate message; the largest list and
// start requests the validation allows must also leave room well under it.
func TestWorstCaseListAndStartMessagesFitTheMessageCap(t *testing.T) {
	started := time.Now()
	worst := Snapshot{OperationID: strings.Repeat("a", 128), State: StateRunning, StartedAt: &started, Result: &Result{Code: -1, Signal: "SIGKILL", ExitedAt: started}, ErrorCode: strings.Repeat("e", 64), NextSequence: 1<<64 - 1}
	snapshots := make([]Snapshot, 1024) // MaximumActive is validated to at most 1024 per owner
	for i := range snapshots {
		snapshots[i] = worst
	}
	list, err := EncodeMessage(remoteResponse{Snapshots: snapshots}, nil)
	if err != nil || len(list) > MaxMessageBytes/8 {
		t.Fatalf("a list of 1024 snapshots is %d bytes (err %v), want room for 8 owners under %d", len(list), err, MaxMessageBytes)
	}
	argv := []string{strings.Repeat("\x01", 64<<10-1)} // validated total 64 KiB; JSON escapes each byte to 6
	environment := map[string]string{}
	for i := 0; i < 16; i++ {
		environment[fmt.Sprintf("K%03d", i)] = strings.Repeat("\x01", 4096)
	}
	start, err := EncodeMessage(remoteRequest{Request: &Request{OperationID: strings.Repeat("o", 128), Argv: argv, CWD: strings.Repeat("c", 4096), Environment: environment}}, nil)
	if err != nil || len(start) > MaxMessageBytes/4 {
		t.Fatalf("the largest start request is %d bytes (err %v), want under %d", len(start), err, MaxMessageBytes/4)
	}
}

func TestMessageBeyondTheCapIsRefusedOnBothSides(t *testing.T) {
	if _, err := EncodeMessage(struct{}{}, make([]byte, MaxMessageBytes)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("oversized message encoded: %v", err)
	}
	huge := make([]byte, 8)
	binary.BigEndian.PutUint32(huge, MaxMessageBytes+1)
	if _, err := DecodeMessage(huge, &struct{}{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("header length beyond the cap accepted: %v", err)
	}
	if _, err := DecodeMessage([]byte{0, 0, 0, 9, '{'}, &struct{}{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("header longer than the frame accepted: %v", err)
	}
}

// MaximumActive is what bounds a list answer: a configuration above the tested worst case never starts.
func TestMaximumActiveAboveTheListBoundIsRefused(t *testing.T) {
	if _, err := New(Config{WorkspaceRoot: t.TempDir(), BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 1025, MaximumOperations: 2048}); err == nil {
		t.Fatal("MaximumActive 1025 accepted: a list could outgrow the message cap")
	}
}
