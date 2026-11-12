package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
)

func publishText(t *testing.T, f *Fanout, next *uint64, data string) {
	t.Helper()
	event := history.Event{Channel: 1, StartSequence: *next, EndSequence: *next + uint64(len(data)), Data: []byte(data)}
	*next = event.EndSequence
	if _, err := f.Publish(event); err != nil {
		t.Fatal(err)
	}
}

// Ctrl-C over a flood sends a stalled or live viewer to a fresh screen (lagged) and keeps what came after it queued;
// a viewer with only a prompt-sized backlog is left alone.
func TestDiscardPendingOutputResyncsViewersWithAFloodBacklog(t *testing.T) {
	f := NewFanout()
	for _, id := range []string{"live", "stalled"} {
		if err := f.Attach(id, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	var next uint64
	publishText(t, f, &next, strings.Repeat("x", interruptDiscardThreshold))
	if err := f.Attach("late", 1<<20); err != nil { // attached after the flood: nothing queued
		t.Fatal(err)
	}
	through := f.PublishedEnd()
	publishText(t, f, &next, "^C\r\n$ ") // the shell's answer, printed before the discard ran
	f.DiscardPendingOutput(through)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, id := range []string{"live", "stalled"} {
		if _, queued, _ := f.Status(id); queued != uint64(len("^C\r\n$ ")) {
			t.Fatalf("%s queued=%d, want only the output after the interrupt", id, queued)
		}
		if _, err := f.WaitNext(ctx, id); !errors.Is(err, ErrAttachmentLagged) {
			t.Fatalf("%s: err=%v, want ErrAttachmentLagged (the screen resync)", id, err)
		}
	}
	event, err := f.WaitNext(ctx, "late")
	if err != nil || string(event.Data) != "^C\r\n$ " {
		t.Fatalf("a viewer with no backlog got %q, %v; want the prompt untouched", event.Data, err)
	}
}

// A backlog under the threshold, printed at a modest rate, drains by itself: no resync, nothing dropped.
func TestDiscardPendingOutputLeavesASmallBacklog(t *testing.T) {
	f := NewFanout()
	if err := f.Attach("live", 1<<20); err != nil {
		t.Fatal(err)
	}
	var next uint64
	publishText(t, f, &next, strings.Repeat("x", interruptDiscardThreshold/2-1))
	f.DiscardPendingOutput(f.PublishedEnd())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if event, err := f.WaitNext(ctx, "live"); err != nil || len(event.Data) != interruptDiscardThreshold/2-1 {
		t.Fatalf("small backlog: %d bytes, %v; want it delivered whole", len(event.Data), err)
	}
}

// A resize queued behind the flood is not skipped: the resync screen would not tell that viewer its new size.
func TestDiscardPendingOutputKeepsAViewerWithAQueuedResizeWhole(t *testing.T) {
	f := NewFanout()
	if err := f.Attach("live", 1<<20); err != nil {
		t.Fatal(err)
	}
	var next uint64
	publishText(t, f, &next, strings.Repeat("x", interruptDiscardThreshold))
	resize := history.Event{Channel: 2, StartSequence: next, EndSequence: next + 4, Data: []byte("size")}
	next = resize.EndSequence
	if _, err := f.Publish(resize); err != nil {
		t.Fatal(err)
	}
	publishText(t, f, &next, strings.Repeat("y", 100))
	f.DiscardPendingOutput(f.PublishedEnd())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got int
	for from := uint64(0); from < next; {
		event, err := f.WaitNextFrom(ctx, "live", from)
		if err != nil {
			t.Fatalf("after %d bytes: %v (the backlog was cut or the viewer resynced)", got, err)
		}
		if event.Channel == 2 {
			got = -1
		}
		from = event.EndSequence
		if got >= 0 {
			got += len(event.Data)
		}
	}
	if got != -1 {
		t.Fatal("the queued resize never reached the viewer")
	}
}

// X-24: under a flood the transport already holds megabytes the cursor has passed, so the queue here is empty at Ctrl-C
// while the viewer is far behind. A viewer with nothing queued is resynced when the program was printing a flood.
func TestDiscardPendingOutputResyncsADrainedViewerUnderAFlood(t *testing.T) {
	f := NewFanout()
	if err := f.Attach("live", 1<<20); err != nil {
		t.Fatal(err)
	}
	var next uint64
	publishText(t, f, &next, strings.Repeat("x", interruptDiscardThreshold))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.WaitNext(ctx, "live"); err != nil { // the stream writer took it: the cursor is at the end
		t.Fatal(err)
	}
	f.DiscardPendingOutput(f.PublishedEnd())
	if !f.Lagged("live") {
		t.Fatal("a drained viewer under a flood was not sent to a fresh screen")
	}
}

// The same viewer at a trickle (a prompt, a progress line) keeps its stream: no resync.
func TestDiscardPendingOutputLeavesADrainedViewerAtATrickle(t *testing.T) {
	f := NewFanout()
	if err := f.Attach("live", 1<<20); err != nil {
		t.Fatal(err)
	}
	var next uint64
	publishText(t, f, &next, "$ sleep 100\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.WaitNext(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	f.DiscardPendingOutput(f.PublishedEnd())
	if f.Lagged("live") {
		t.Fatal("a quiet viewer was resynced")
	}
}

// The flood rate counts for a viewer that falls behind and has its backlog dropped (the case that matters): the
// overflow path must not skip it.
func TestDiscardPendingOutputCountsAViewerThatOverflowed(t *testing.T) {
	f := NewFanout()
	if err := f.Attach("live", 1<<16); err != nil { // a 64 KiB queue that the flood overflows
		t.Fatal(err)
	}
	var next uint64
	publishText(t, f, &next, strings.Repeat("x", 1<<17)) // overflows at once: backlog dropped, viewer lagged
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.WaitNext(ctx, "live"); !errors.Is(err, ErrAttachmentLagged) { // the screen was sent: lagged is cleared
		t.Fatalf("setup: %v", err)
	}
	f.DiscardPendingOutput(f.PublishedEnd())
	if !f.Lagged("live") {
		t.Fatal("a viewer that overflowed under a flood was not resynced at Ctrl-C")
	}
}

// X-24: a lone Ctrl-C the worker writes normally (nothing stuck on input) used to be answered "nothing discarded" whatever
// the flood: `yes` and Ctrl-C painted 19 MB for 60 s. The answer now cuts a flooded viewer and leaves a quiet one alone.
func TestAnswerInterruptCutsAFloodedViewerWhoseKeyWasWrittenNormally(t *testing.T) {
	for _, c := range []struct {
		name    string
		bytes   int
		discard bool
	}{{"flood", interruptDiscardThreshold, true}, {"prompt", 20, false}} {
		t.Run(c.name, func(t *testing.T) {
			s := &managedSession{fanout: NewFanout()}
			if err := s.fanout.Attach("live", 1<<20); err != nil {
				t.Fatal(err)
			}
			var next uint64
			publishText(t, s.fanout, &next, strings.Repeat("x", c.bytes))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := s.fanout.WaitNext(ctx, "live"); err != nil {
				t.Fatal(err)
			}
			var decision InputDecision
			s.answerInterrupt(InputKey{AttachmentID: "live", InputSequence: 5}, &decision)
			if decision.InterruptDiscarded != c.discard || decision.InterruptThrough != s.fanout.PublishedEnd() {
				t.Fatalf("discarded=%v through=%d, want discarded=%v through=%d", decision.InterruptDiscarded, decision.InterruptThrough, c.discard, s.fanout.PublishedEnd())
			}
		})
	}
}
