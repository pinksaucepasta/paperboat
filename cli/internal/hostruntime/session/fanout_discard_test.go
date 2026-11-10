package session

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
)

// Ctrl-C drops the flood queued for every viewer, keeps a queued resize, and delivers later output.
func TestDiscardPendingOutputKeepsControlAndLaterOutput(t *testing.T) {
	f := NewFanout()
	for _, id := range []string{"live", "stalled"} {
		if err := f.Attach(id, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	var next uint64
	publish := func(channel byte, data string) {
		event := history.Event{Channel: channel, StartSequence: next, EndSequence: next + uint64(len(data)), Data: []byte(data)}
		next = event.EndSequence
		if _, err := f.Publish(event); err != nil {
			t.Fatal(err)
		}
	}
	publish(1, "flood1")
	publish(1, "flood2")
	publish(2, "resize")
	publish(1, "flood3")
	f.DiscardPendingOutput()
	for _, id := range []string{"live", "stalled"} {
		if _, queued, _ := f.Status(id); queued != uint64(len("resize")+len("flood3")) {
			t.Fatalf("%s queued=%d", id, queued)
		}
	}
	publish(1, "prompt")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got []string
	for range 3 {
		event, err := f.WaitNext(ctx, "live")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(event.Data))
		_, _ = f.WaitNextFrom(ctx, "live", event.EndSequence)
	}
	if got[0] != "resize" || got[1] != "flood3" || got[2] != "prompt" {
		t.Fatalf("got %q", got)
	}
}
