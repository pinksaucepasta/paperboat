package tunnel

import (
	"bytes"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"testing"
	"time"
)

func holdConn() *helperTerminalConn {
	return &helperTerminalConn{out: make(chan helperOutput, 256), done: make(chan struct{})}
}

// frame is output covering [start, start+len(text)).
func frame(start uint64, text string) helperOutput {
	return helperOutput{byteCursor: true, data: []byte(text), endSequence: start + uint64(len(text))}
}

func drain(c *helperTerminalConn) []helperOutput {
	var got []helperOutput
	for {
		select {
		case output := <-c.out:
			got = append(got, output)
		default:
			return got
		}
	}
}

func joined(outputs []helperOutput) string {
	var b bytes.Buffer
	for _, output := range outputs {
		b.Write(output.data)
	}
	return b.String()
}

// Not discarded (an idle prompt, a small backlog, a Ctrl-C that was data): everything held comes back byte for
// byte and in order: no repaint, nothing dropped.
func TestHoldReleasesUnchangedWhenNothingWasDiscarded(t *testing.T) {
	c := holdConn()
	c.startHold(7)
	for i, text := range []string{"aa", "bb", "cc"} {
		c.routeOutput(frame(uint64(i*2), text))
	}
	if got := drain(c); len(got) != 0 {
		t.Fatalf("%d outputs passed while the Ctrl-C was unanswered", len(got))
	}
	c.resolveHold(7, 6, false)
	if got := joined(drain(c)); got != "aabbcc" {
		t.Fatalf("released %q, want everything unchanged", got)
	}
	c.routeOutput(frame(6, "dd"))
	if got := joined(drain(c)); got != "dd" {
		t.Fatalf("after the hold %q passed, want dd", got)
	}
}

// Discarded: the screen passes at once and the held output up to the cut is dropped; output of the same stream
// is in sequence order, so what still arrives up to the cut after the answer is dropped, and the first output past it
// ends that.
func TestHoldDropsWhatTheHostDiscardedAndKeepsTheRest(t *testing.T) {
	c := holdConn()
	c.startHold(7)
	c.routeOutput(frame(0, "flood1"))
	c.routeOutput(helperOutput{checkpoint: true, data: []byte("SCREEN"), endSequence: 12})
	if got := joined(drain(c)); got != "SCREEN" {
		t.Fatalf("while held %q passed, want only the screen", got)
	}
	c.resolveHold(7, 12, true)
	if got := drain(c); len(got) != 0 {
		t.Fatalf("the discarded output was released: %q", joined(got))
	}
	c.routeOutput(frame(6, "flood2")) // still in flight when the answer came: ends at 12
	if got := drain(c); len(got) != 0 {
		t.Fatalf("a late frame up to the cut was painted: %q", joined(got))
	}
	c.routeOutput(frame(12, "after"))
	c.routeOutput(frame(17, "next"))
	if got := joined(drain(c)); got != "afternext" {
		t.Fatalf("after the cut %q passed, want afternext", got)
	}
}

// Output past the cut that was held when the answer came is kept (the shell's answer to the key).
func TestHoldKeepsOutputPastTheCutHeldBeforeTheAnswer(t *testing.T) {
	c := holdConn()
	c.startHold(7)
	c.routeOutput(frame(0, "flood"))
	c.routeOutput(frame(5, "prompt"))
	c.resolveHold(7, 5, true)
	if got := joined(drain(c)); got != "prompt" {
		t.Fatalf("released %q, want only the output past the cut", got)
	}
}

// A frame that straddles the cut keeps only its part after it.
func TestHoldTrimsAFrameAcrossTheCut(t *testing.T) {
	c := holdConn()
	c.startHold(7)
	c.routeOutput(frame(8, "01234567")) // [8,16)
	c.resolveHold(7, 12, true)
	if got := joined(drain(c)); got != "4567" {
		t.Fatalf("released %q, want the part after the cut", got)
	}
}

func TestHoldReleasesOnTimeout(t *testing.T) {
	previous := helperHoldTimeout
	helperHoldTimeout = 50 * time.Millisecond
	defer func() { helperHoldTimeout = previous }()
	c := holdConn()
	c.startHold(7)
	c.routeOutput(frame(0, "held"))
	time.Sleep(300 * time.Millisecond)
	if got := joined(drain(c)); got != "held" {
		t.Fatalf("after the timeout %q passed, want held", got)
	}
	c.routeOutput(frame(4, "live"))
	if got := joined(drain(c)); got != "live" {
		t.Fatalf("output is still held after the timeout: %q", got)
	}
}

func TestHoldReleasesWhenTheBufferIsFull(t *testing.T) {
	c := holdConn()
	c.startHold(7)
	big := bytes.Repeat([]byte("x"), helperHoldMaxBytes/2+1)
	c.routeOutput(helperOutput{byteCursor: true, data: big, endSequence: uint64(len(big))})
	if len(drain(c)) != 0 {
		t.Fatal("output passed before the buffer was full")
	}
	c.routeOutput(helperOutput{byteCursor: true, data: big, endSequence: 2 * uint64(len(big))})
	if got := drain(c); len(got) != 2 {
		t.Fatalf("%d outputs after overflow, want both released in order", len(got))
	}
}

func TestHoldIgnoresAnAnswerForAnotherKeyAndKeepsHoldingForTheNewest(t *testing.T) {
	c := holdConn()
	c.startHold(7)
	c.routeOutput(frame(0, "a"))
	c.startHold(9) // a second Ctrl-C supersedes the first; what was held stays held
	c.resolveHold(7, 1, true)
	if got := drain(c); len(got) != 0 {
		t.Fatal("an answer for the superseded key ended the hold")
	}
	c.resolveHold(9, 1, false)
	if got := joined(drain(c)); got != "a" {
		t.Fatalf("released %q, want a", got)
	}
}

func TestFlushHoldOnStreamEndReleasesTheHeldOutput(t *testing.T) {
	c := holdConn()
	c.startHold(7)
	c.routeOutput(frame(0, "tail"))
	c.flushHold()
	if got := joined(drain(c)); got != "tail" {
		t.Fatalf("released %q on stream end, want tail", got)
	}
}

// X-24: output already queued for Read when a discarded answer arrives (up to 16 MiB after a flood) is dropped too, not
// painted for as long as the flood took to print; the screen and what follows the cut are kept.
func TestHoldDropsOutputAlreadyQueuedForRead(t *testing.T) {
	c := holdConn()
	c.target = &resolver.TerminalTarget{}
	c.ackNotify = make(chan struct{}, 1)
	c.out <- frame(0, "flood1")
	c.out <- frame(6, "flood2")
	c.startHold(7)
	c.routeOutput(frame(12, "after")) // held, and past the cut: it ends the cut for arrivals, not for what is queued ahead of it
	c.resolveHold(7, 12, true)        // the cut is the end of flood2
	c.out <- helperOutput{checkpoint: true, data: []byte("SCREEN"), endSequence: 12}
	var got bytes.Buffer
	buf := make([]byte, 64)
	for got.Len() < len("afterSCREEN") {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got.Write(buf[:n])
	}
	if got.String() != "afterSCREEN" {
		t.Fatalf("read %q, want the output after the cut and the screen only", got.String())
	}
}
