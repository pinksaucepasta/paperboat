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

// The header cap equals the bridge's body cap, so it can never refuse a legitimate message. The largest valid start
// request (arguments and environment at their validated maximum, every byte a control byte that JSON escapes six-fold,
// plus the largest managed environment) must still leave room under it.
func TestWorstCaseStartMessageFitsTheMessageCap(t *testing.T) {
	argv := make([]string, 16) // validated: each <= 4096 bytes, 64 KiB in total
	for i := range argv {
		argv[i] = strings.Repeat("\x01", 4096)
	}
	environment := map[string]string{}
	for i := 0; i < 16; i++ { // validated: key+value bytes <= 64 KiB in total
		environment[fmt.Sprintf("K%03d", i)] = strings.Repeat("\x01", 4092)
	}
	managed := make([]string, 272) // validBaseEnvironment: 272 entries, 320 KiB
	for i := range managed {
		managed[i] = fmt.Sprintf("M%03d=", i) + strings.Repeat("\x01", 320<<10/272-5)
	}
	start, err := EncodeMessage(remoteRequest{ManagedEnvironment: managed, Request: &Request{OperationID: strings.Repeat("o", 128), Argv: argv, CWD: strings.Repeat("c", 4096), Environment: environment}}, nil)
	if err != nil || len(start) > MaxMessageBytes*2/5 {
		t.Fatalf("the largest start request is %d bytes (err %v), want under %d of %d", len(start), err, MaxMessageBytes*2/5, MaxMessageBytes)
	}
}

// A list is answered in pages, so no number of owners or active commands can outgrow one message.
func TestListIsPagedAndRemoteManagerReadsEveryPage(t *testing.T) {
	var all []Snapshot
	for i := 0; i < 2*listPageSnapshots+17; i++ {
		all = append(all, Snapshot{OperationID: fmt.Sprintf("operation_%06d", i), State: StateRunning})
	}
	server := &RemoteServer{Manager: listing{snapshots: all}}
	pages := 0
	remote, _ := NewRemoteManager(func(ctx context.Context, op string, body []byte) ([]byte, error) {
		if op == "list" {
			pages++
		}
		return server.Call(ctx, op, body)
	})
	got := remote.ActiveSnapshots()
	if len(got) != len(all) || pages != 3 {
		t.Fatalf("got %d of %d snapshots in %d pages, want all in 3", len(got), len(all), pages)
	}
	for i := range got {
		if got[i].OperationID != all[i].OperationID {
			t.Fatalf("snapshot %d is %s, want %s", i, got[i].OperationID, all[i].OperationID)
		}
	}
	page, more := ListPage(all, uint64(len(all)+5))
	if len(page) != 0 || more {
		t.Fatalf("a page past the end holds %d, more=%v", len(page), more)
	}
	// Whatever fills the pages, one answer stays far under the cap even for the largest snapshots.
	worst := Snapshot{OperationID: strings.Repeat("a", 128), State: StateRunning, StartedAt: &time.Time{}, Result: &Result{Signal: "SIGKILL"}, ErrorCode: strings.Repeat("e", 64), NextSequence: 1<<64 - 1}
	page = make([]Snapshot, listPageSnapshots)
	for i := range page {
		page[i] = worst
	}
	if message, err := EncodeMessage(remoteResponse{Snapshots: page, More: true}, nil); err != nil || len(message) > MaxMessageBytes/4 {
		t.Fatalf("a full page is %d bytes (err %v)", len(message), err)
	}
}

type listing struct {
	Service
	snapshots []Snapshot
}

func (l listing) ActiveSnapshots() []Snapshot { return l.snapshots }

func TestMessageBeyondTheCapIsRefusedOnBothSides(t *testing.T) {
	if _, err := EncodeMessage(struct{}{}, make([]byte, MaxMessageBytes)); !errors.Is(err, ErrMessageTooLarge) || errors.Is(err, ErrCapacity) {
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
