package filetransfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
)

func shortCheckpoints(t *testing.T) {
	t.Helper()
	previous := checkpointBytes
	checkpointBytes = 1024
	t.Cleanup(func() { checkpointBytes = previous })
}

func patternData(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	return data
}

// watchedBody delivers data in small reads and lets a hook run before each one.
type watchedBody struct {
	data   []byte
	before func(delivered int)
	fail   error
}

func (b *watchedBody) Read(p []byte) (int, error) {
	if b.before != nil {
		b.before(len(b.data))
	}
	if len(b.data) == 0 {
		if b.fail != nil {
			return 0, b.fail
		}
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 512)], b.data)
	b.data = b.data[n:]
	return n, nil
}

func committed(t *testing.T, durable *store.Store, id string) int64 {
	t.Helper()
	transfer, err := durable.FileTransfer(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.CommittedOffset
}

func TestSegmentStreamsOneBodyAndCommitsAtCheckpoints(t *testing.T) {
	shortCheckpoints(t)
	service, durable, _ := newService(t)
	data := patternData(5000)
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	var seen []int64
	body := &watchedBody{data: data}
	body.before = func(int) {
		if offset := committed(t, durable, id); len(seen) == 0 || seen[len(seen)-1] != offset {
			seen = append(seen, offset)
		}
	}
	got, err := service.AppendSegment(context.Background(), id, 0, body)
	if err != nil || got.CommittedOffset != int64(len(data)) {
		t.Fatalf("segment=%#v err=%v", got, err)
	}
	if len(seen) < 4 || seen[0] != 0 || seen[1] != 1024 || seen[2] != 2048 {
		t.Fatalf("offsets committed while the body was still arriving: %v", seen)
	}
	if done, err := service.Complete(context.Background(), id); err != nil || done.State != "published" {
		t.Fatalf("complete=%#v err=%v", done, err)
	}
}

func TestSegmentCutKeepsEverythingThatArrivedAndResumes(t *testing.T) {
	shortCheckpoints(t)
	service, durable, _ := newService(t)
	data := patternData(5000)
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	cut := errors.New("connection reset")
	_, err = service.AppendSegment(context.Background(), id, 0, &watchedBody{data: data[:3000], fail: cut})
	if err == nil {
		t.Fatal("a cut body must be an error")
	}
	if offset := committed(t, durable, id); offset != 3000 {
		t.Fatalf("committed %d after a cut at 3000: the resume would resend what arrived", offset)
	}
	if _, err := service.AppendSegment(context.Background(), id, 3000, bytes.NewReader(data[3000:])); err != nil {
		t.Fatal(err)
	}
	if done, err := service.Complete(context.Background(), id); err != nil || done.State != "published" {
		t.Fatalf("complete=%#v err=%v", done, err)
	}
}

func TestSegmentCutByTheCallerStillKeepsTheArrivedBytes(t *testing.T) {
	shortCheckpoints(t)
	service, durable, _ := newService(t)
	data := patternData(5000)
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	ctx, cancel := context.WithCancel(context.Background())
	body := &watchedBody{data: data}
	body.before = func(left int) {
		if left <= len(data)-2500 {
			cancel()
		}
	}
	if _, err := service.AppendSegment(ctx, id, 0, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if offset := committed(t, durable, id); offset < 2560 || offset > 3072 {
		t.Fatalf("committed %d after the caller left at ~2500", offset)
	}
}

func TestSegmentIsBoundedAndKeepsItsValidPrefix(t *testing.T) {
	service, durable, _ := newService(t)
	size := int64(protocol.FileTransferSegmentBytes) + 4096
	request := requestFor(nil)
	request.Files[0].Size = size
	created, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	_, err = service.AppendSegment(context.Background(), id, 0, io.LimitReader(zeros{}, size))
	if !hasCode(err, InvalidSize) {
		t.Fatalf("a segment longer than the bound: %v", err)
	}
	if offset := committed(t, durable, id); offset != protocol.FileTransferSegmentBytes {
		t.Fatalf("committed %d, want the segment bound", offset)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestSegmentStopsAtItsNextCheckpointWhenAnUpdateBegins(t *testing.T) {
	shortCheckpoints(t)
	service, durable, _ := newService(t)
	data := patternData(5000)
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	begun := make(chan error, 1)
	started := false
	body := &watchedBody{data: data}
	body.before = func(int) {
		if started {
			return
		}
		started = true
		go func() { begun <- service.BeginUpdate(context.Background(), "tx") }()
		for !service.updating() {
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := service.AppendSegment(context.Background(), id, 0, body); !hasCode(err, UpdateInProgress) {
		t.Fatalf("err=%v", err)
	}
	select {
	case err := <-begun:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the update was never admitted")
	}
	if offset := committed(t, durable, id); offset != 1024 {
		t.Fatalf("committed %d, want the first checkpoint", offset)
	}
	if err := service.EndUpdate("tx"); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentOnASlowLinkCommitsByTimeNotOnlyByBytes(t *testing.T) {
	previousBytes, previousInterval, previousStep := checkpointBytes, checkpointInterval, copyStepBytes
	checkpointBytes, checkpointInterval, copyStepBytes = 1<<30, 20*time.Millisecond, 512
	t.Cleanup(func() {
		checkpointBytes, checkpointInterval, copyStepBytes = previousBytes, previousInterval, previousStep
	})
	service, durable, _ := newService(t)
	data := patternData(4096)
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	var seen int64
	body := &watchedBody{data: data}
	body.before = func(left int) {
		time.Sleep(5 * time.Millisecond)
		if left > 0 {
			seen = max(seen, committed(t, durable, id))
		}
	}
	if _, err := service.AppendSegment(context.Background(), id, 0, body); err != nil {
		t.Fatal(err)
	}
	if seen == 0 || seen >= int64(len(data)) {
		t.Fatalf("highest offset committed before the end: %d", seen)
	}
}
