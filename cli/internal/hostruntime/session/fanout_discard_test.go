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

// A backlog under the threshold drains by itself: no resync, nothing dropped.
func TestDiscardPendingOutputLeavesASmallBacklog(t *testing.T) {
	f := NewFanout()
	if err := f.Attach("live", 1<<20); err != nil {
		t.Fatal(err)
	}
	var next uint64
	publishText(t, f, &next, strings.Repeat("x", interruptDiscardThreshold-1))
	f.DiscardPendingOutput(f.PublishedEnd())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if event, err := f.WaitNext(ctx, "live"); err != nil || len(event.Data) != interruptDiscardThreshold-1 {
		t.Fatalf("small backlog: %d bytes, %v; want it delivered whole", len(event.Data), err)
	}
}
