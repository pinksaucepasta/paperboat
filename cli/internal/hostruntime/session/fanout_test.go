package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
)

func TestWaitNextWakesForOutputDetachEvictionAndCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		action func(*Fanout)
		want   error
	}{
		{"output", func(f *Fanout) {
			_, _ = f.Publish(history.Event{Channel: 1, StartSequence: 0, EndSequence: 1, Data: []byte("x")})
		}, nil},
		{"detach", func(f *Fanout) { _ = f.Detach("att") }, ErrAttachmentUnknown},
		{"evict", func(f *Fanout) {
			_, _ = f.Publish(history.Event{Channel: 1, StartSequence: 0, EndSequence: 2, Data: []byte("xx")})
		}, ErrAttachmentEvicted},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fanout := NewFanout()
			limit := uint64(1)
			if testCase.name == "output" || testCase.name == "detach" {
				limit = 8
			}
			// Backpressured: only the owner is ever evicted (a link that stopped draining).
			if err := fanout.AttachBackpressured("att", limit); err != nil {
				t.Fatal(err)
			}
			fanout.SetController("att")
			done := make(chan error, 1)
			go func() {
				event, err := fanout.WaitNext(context.Background(), "att")
				if err == nil && string(event.Data) != "x" {
					err = ErrOutputOrder
				}
				done <- err
			}()
			testCase.action(fanout)
			if err := <-done; !errors.Is(err, testCase.want) {
				t.Fatalf("err=%v want=%v", err, testCase.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	fanout := NewFanout()
	_ = fanout.Attach("att", 8)
	cancel()
	if _, err := fanout.WaitNext(ctx, "att"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
}

func TestPublishOwnedSharesPayloadAcrossAttachments(t *testing.T) {
	for _, attachmentCount := range []int{1, 4, 16} {
		t.Run(fmt.Sprintf("attachments_%d", attachmentCount), func(t *testing.T) {
			f := NewFanout()
			ids := make([]string, attachmentCount)
			for index := range ids {
				ids[index] = fmt.Sprintf("att_%d", index)
				if err := f.Attach(ids[index], 4096); err != nil {
					t.Fatal(err)
				}
			}
			payload := []byte("shared")
			if _, err := f.PublishOwned(history.Event{Channel: 1, StartSequence: 0, EndSequence: uint64(len(payload)), Data: payload}); err != nil {
				t.Fatal(err)
			}
			if len(f.ring) != 1 {
				t.Fatalf("ring events=%d want=1", len(f.ring))
			}
			for _, id := range ids {
				event, err := f.WaitNextOwned(context.Background(), id)
				if err != nil || &event.Data[0] != &payload[0] {
					t.Fatalf("%s payload was cloned: event=%p source=%p err=%v", id, event.Data, payload, err)
				}
				event.Release()
			}
			if len(f.ring) != 0 {
				t.Fatalf("drained ring events=%d want=0", len(f.ring))
			}
		})
	}
}

func TestFanoutCompactsBehindSlowestAttachedCursor(t *testing.T) {
	f := NewFanout()
	for _, id := range []string{"fast", "slow"} {
		if err := f.Attach(id, 16); err != nil {
			t.Fatal(err)
		}
	}
	for sequence, value := range []byte("abc") {
		start := uint64(sequence)
		if _, err := f.PublishOwned(history.Event{StartSequence: start, EndSequence: start + 1, Data: []byte{value}}); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		event, err := f.WaitNextOwned(context.Background(), "fast")
		if err != nil {
			t.Fatal(err)
		}
		event.Release()
	}
	if len(f.ring) != 3 {
		t.Fatalf("ring compacted ahead of slow cursor: events=%d", len(f.ring))
	}
	for remaining := 2; remaining >= 0; remaining-- {
		event, err := f.WaitNextOwned(context.Background(), "slow")
		if err != nil {
			t.Fatal(err)
		}
		event.Release()
		if len(f.ring) != remaining {
			t.Fatalf("ring events=%d want=%d", len(f.ring), remaining)
		}
	}
}

func BenchmarkPublishOwnedAttachments(b *testing.B) {
	for _, attachments := range []int{1, 4, 16, 256, 1024} { // a shared terminal with many viewers
		b.Run(fmt.Sprintf("%d", attachments), func(b *testing.B) {
			f := NewFanout()
			ids := make([]string, attachments)
			for index := 0; index < attachments; index++ {
				ids[index] = fmt.Sprintf("att_%d", index)
				if err := f.Attach(ids[index], 1<<20); err != nil {
					b.Fatal(err)
				}
			}
			payload := make([]byte, 4096)
			b.ReportAllocs()
			b.SetBytes(int64(len(payload) * attachments))
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				start := uint64(index * len(payload))
				if _, err := f.PublishOwned(history.Event{Channel: 1, StartSequence: start, EndSequence: start + uint64(len(payload)), Data: payload}); err != nil {
					b.Fatal(err)
				}
				for attachment := 0; attachment < attachments; attachment++ {
					if _, err := f.WaitNextOwned(context.Background(), ids[attachment]); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func BenchmarkOutputBurst1MiB(b *testing.B) {
	const chunkSize = 32 << 10
	payload := make([]byte, chunkSize)
	for _, attachments := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("attachments_%d", attachments), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(1 << 20)
			for range b.N {
				fanout := NewFanout()
				ids := make([]string, attachments)
				for index := range ids {
					ids[index] = fmt.Sprintf("att_%d", index)
					if err := fanout.Attach(ids[index], 1<<20); err != nil {
						b.Fatal(err)
					}
				}
				for start := uint64(0); start < 1<<20; start += chunkSize {
					event := history.Event{Channel: 1, StartSequence: start, EndSequence: start + chunkSize, Data: payload}
					if _, err := fanout.PublishOwned(event); err != nil {
						b.Fatal(err)
					}
				}
				for _, id := range ids {
					for range (1 << 20) / chunkSize {
						if _, err := fanout.WaitNextOwned(context.Background(), id); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
		})
	}
}

func TestFanoutMirrorsOrderedOutput(t *testing.T) {
	f := NewFanout()
	for _, id := range []string{"att_1", "att_2"} {
		if err := f.Attach(id, 16); err != nil {
			t.Fatal(err)
		}
	}
	event := history.Event{Channel: 1, StartSequence: 0, EndSequence: 3, Data: []byte("abc")}
	if evictions, err := f.Publish(event); err != nil || len(evictions) != 0 {
		t.Fatalf("evictions=%v err=%v", evictions, err)
	}
	event.Data[0] = 'x'
	for _, id := range []string{"att_1", "att_2"} {
		got, ok, err := f.Next(id)
		if err != nil || !ok || string(got.Data) != "abc" {
			t.Fatalf("%s event=%#v ok=%v err=%v", id, got, ok, err)
		}
	}
}

func TestSlowConsumerEvictsOnlyFullAttachment(t *testing.T) {
	f := NewFanout()
	f.AttachBackpressured("slow", 3)
	f.AttachBackpressured("fast", 3)
	f.SetController("slow") // only the controller is ever evicted: a link that stopped draining
	f.Publish(history.Event{StartSequence: 0, EndSequence: 3, Data: []byte("abc")})
	if _, ok, err := f.Next("fast"); err != nil || !ok {
		t.Fatalf("fast drain ok=%v err=%v", ok, err)
	}
	evictions, err := f.Publish(history.Event{StartSequence: 3, EndSequence: 4, Data: []byte("d")})
	if err != nil || len(evictions) != 1 || evictions[0].AttachmentID != "slow" || evictions[0].QueuedBytes != 3 {
		t.Fatalf("evictions=%#v err=%v", evictions, err)
	}
	if _, _, err := f.Next("slow"); !errors.Is(err, ErrAttachmentEvicted) {
		t.Fatalf("slow err=%v", err)
	}
	if state, queued, statusErr := f.Status("slow"); statusErr != nil || state != Evicted || queued != 3 {
		t.Fatalf("slow state=%s queued=%d err=%v", state, queued, statusErr)
	}
	if f.IsAttached("slow") || !f.IsAttached("fast") {
		t.Fatalf("lock-free attachment index slow=%v fast=%v", f.IsAttached("slow"), f.IsAttached("fast"))
	}
	got, ok, err := f.Next("fast")
	if err != nil || !ok || string(got.Data) != "d" {
		t.Fatalf("fast=%#v ok=%v err=%v", got, ok, err)
	}
}

func TestDetachDoesNotAffectOtherAttachments(t *testing.T) {
	f := NewFanout()
	f.Attach("gone", 8)
	f.Attach("live", 8)
	if err := f.Detach("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Publish(history.Event{StartSequence: 10, EndSequence: 11, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Status("gone"); !errors.Is(err, ErrAttachmentUnknown) {
		t.Fatalf("detached status err=%v", err)
	}
	if _, ok, err := f.Next("live"); err != nil || !ok {
		t.Fatalf("live ok=%v err=%v", ok, err)
	}
}

func TestDetachReleasesAttachmentIdentityAndCapacity(t *testing.T) {
	fanout := NewFanout()
	if err := fanout.Attach("att", 8); err != nil {
		t.Fatal(err)
	}
	if fanout.Count() != 1 {
		t.Fatalf("count=%d", fanout.Count())
	}
	if !fanout.IsAttached("att") {
		t.Fatal("attached identity missing from lock-free index")
	}
	if err := fanout.Detach("att"); err != nil {
		t.Fatal(err)
	}
	if fanout.Count() != 0 {
		t.Fatalf("detached count=%d", fanout.Count())
	}
	if fanout.IsAttached("att") {
		t.Fatal("detached identity remained active")
	}
	if err := fanout.Attach("att", 8); err != nil {
		t.Fatalf("reuse err=%v", err)
	}
}

func TestFanoutRejectsNoncontiguousOutputWithoutPartialDelivery(t *testing.T) {
	f := NewFanout()
	f.Attach("one", 8)
	f.Publish(history.Event{StartSequence: 0, EndSequence: 1, Data: []byte("a")})
	if _, err := f.Publish(history.Event{StartSequence: 2, EndSequence: 3, Data: []byte("b")}); !errors.Is(err, ErrOutputOrder) {
		t.Fatalf("err=%v", err)
	}
	_, queued, _ := f.Status("one")
	if queued != 1 {
		t.Fatalf("queued=%d", queued)
	}
}

// A terminal that stops releases its output buffers, but an attached viewer still receives the output
// it was owed (the shell's last words before exit); what no attached cursor needs is freed at once, and
// the rest as soon as it is read (Lane G: browser broadcast publisher stopped on ErrOutputOrder).
func TestFanoutReleaseKeepsOutputOwedToAttachedViewers(t *testing.T) {
	f := NewFanout()
	for _, id := range []string{"read", "owed"} {
		if err := f.Attach(id, 16); err != nil {
			t.Fatal(err)
		}
	}
	for sequence, value := range []byte("abc") {
		start := uint64(sequence)
		if _, err := f.PublishOwned(history.Event{StartSequence: start, EndSequence: start + 1, Data: []byte{value}}); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		event, err := f.WaitNextOwned(context.Background(), "read")
		if err != nil {
			t.Fatal(err)
		}
		event.Release()
	}
	event, err := f.WaitNextOwned(context.Background(), "owed")
	if err != nil {
		t.Fatal(err)
	}
	event.Release()
	f.Release()
	if len(f.ring) != 2 {
		t.Fatalf("release kept %d events, want the 2 still owed", len(f.ring))
	}
	var tail []byte
	for range 2 {
		event, err := f.WaitNextOwned(context.Background(), "owed")
		if err != nil {
			t.Fatalf("owed output after release: %v", err)
		}
		tail = append(tail, event.Data...)
		event.Release()
	}
	if string(tail) != "bc" || len(f.ring) != 0 {
		t.Fatalf("tail=%q ring=%d", tail, len(f.ring))
	}
}

// A free-running attachment (a viewer, the browser publisher) that falls behind is never evicted: its backlog is
// dropped, it is told once that it lagged, and it continues with the next output.
func TestFreeRunningAttachmentLagsInsteadOfEviction(t *testing.T) {
	f := NewFanout()
	f.Attach("viewer", 3)
	f.Publish(history.Event{StartSequence: 0, EndSequence: 3, Data: []byte("abc")})
	if evictions, err := f.Publish(history.Event{StartSequence: 3, EndSequence: 4, Data: []byte("d")}); err != nil || len(evictions) != 0 {
		t.Fatalf("evictions=%#v err=%v", evictions, err)
	}
	if _, _, err := f.Next("viewer"); !errors.Is(err, ErrAttachmentLagged) {
		t.Fatalf("lagging viewer: %v", err)
	}
	f.Publish(history.Event{StartSequence: 4, EndSequence: 5, Data: []byte("e")})
	if event, ok, err := f.Next("viewer"); err != nil || !ok || string(event.Data) != "e" || event.StartSequence != 4 {
		t.Fatalf("after the lag: %q at %d ok=%v err=%v", event.Data, event.StartSequence, ok, err)
	}
	if state, _, err := f.Status("viewer"); err != nil || state != Attached {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

// Of two owner terminals, only the controller the size rule names paces output. A stalled second window never
// holds the program back, and moving control releases it.
func TestOnlyTheControllingOwnerPacesOutput(t *testing.T) {
	f := NewFanout()
	f.AttachBackpressured("first", 3)
	f.AttachBackpressured("second", 3)
	f.SetController("first") // the size rule's controller
	f.Publish(history.Event{StartSequence: 0, EndSequence: 3, Data: []byte("abc")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	f.WaitRoom(ctx, 1, 300*time.Millisecond) // "first" is full and is the controller: waits
	if time.Since(started) < 250*time.Millisecond {
		t.Fatal("the controller's full queue did not hold the producer")
	}
	f.SetController("second")
	if _, ok, err := f.Next("second"); err != nil || !ok {
		t.Fatalf("second drain ok=%v err=%v", ok, err)
	}
	started = time.Now()
	f.WaitRoom(ctx, 1, 300*time.Millisecond) // "first" is full but no longer the controller
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("a stalled non-controller held the producer")
	}
	if evictions, err := f.Publish(history.Event{StartSequence: 3, EndSequence: 4, Data: []byte("d")}); err != nil || len(evictions) != 0 {
		t.Fatalf("the stalled non-controller was evicted: %#v %v", evictions, err)
	}
	if _, _, err := f.Next("first"); !errors.Is(err, ErrAttachmentLagged) {
		t.Fatalf("stalled non-controller: %v, want lagged", err)
	}
}
