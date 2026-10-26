//go:build js && wasm

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"sync"
	"syscall/js"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// floodHarness drives readBroadcast the way a page sees a flood: sealed records arrive faster than they are drawn,
// the edge drops some, and the machine answers each screen request with a checkpoint (finishResync).
type floodHarness struct {
	t          *testing.T
	connection *browserTerminal
	ws         *browserWSConn
	epoch      browserbroadcast.Epoch
	private    ed25519.PrivateKey
	mu         sync.Mutex
	events     map[string]int
	next       uint64 // end of the last output event drawn
	screen     uint64 // boundary of the last screen answered
}

func newFloodHarness(t *testing.T) *floodHarness {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := browserbroadcast.NewEpoch()
	if err != nil {
		t.Fatal(err)
	}
	h := &floodHarness{t: t, epoch: epoch, private: private, events: map[string]int{}}
	callback := js.FuncOf(func(_ js.Value, args []js.Value) any {
		event := args[0]
		kind := event.Get("type").String()
		h.mu.Lock()
		h.events[kind]++
		if kind == "output" {
			fmt.Sscan(event.Get("next_sequence").String(), &h.next)
		}
		h.mu.Unlock()
		return nil
	})
	t.Cleanup(callback.Release)
	socket := js.Global().Get("Object").New()
	socket.Set("readyState", 3)
	h.ws = &browserWSConn{socket: socket, broadcasts: newTerminalReceiveQueue(), done: make(chan struct{}), failure: make(chan error, 1), opened: make(chan struct{})}
	h.connection = &browserTerminal{
		id: "connection_test", callback: callback.Value, config: terminalConfig{TerminalSessionID: "session_test", Role: "owner"},
		writes: newTerminalWriterQueue(), keyEvents: make(chan browserbroadcast.Epoch, 4), resynced: make(chan uint64, 1),
		attached: true, tls: &tls.Conn{}, websocket: h.ws,
	}
	h.connection.keyEvents <- epoch
	go func() { // the write loop: requests leave for the machine
		for {
			select {
			case item := <-h.connection.writes.items:
				h.connection.writes.release(item)
			case <-h.ws.done:
				return
			}
		}
	}()
	go h.connection.readBroadcast(h.ws, public, 1, 0)
	t.Cleanup(func() { h.ws.finish(nil) })
	return h
}

// send seals one record and offers it to the receive queue as the WebSocket reader does (a full queue drops it).
func (h *floodHarness) send(index, start uint64, data []byte) {
	h.offer(index, h.seal(index, start, data))
}

func (h *floodHarness) seal(index, start uint64, data []byte) []byte {
	record, err := browserbroadcast.Seal(browserbroadcast.Record{SessionID: "session_test", Generation: 1, EpochID: h.epoch.ID, Index: index, Channel: protocol.TerminalStdout, StartSequence: start, Data: data}, h.epoch, h.private)
	if err != nil {
		h.t.Fatal(err)
	}
	return record
}

func (h *floodHarness) offer(index uint64, record []byte) {
	if err := h.ws.broadcasts.copyAndEnqueue(len(record), func(out []byte) bool { return copy(out, record) == len(record) }); err != nil {
		select {
		case cause := <-h.ws.failure:
			h.t.Fatalf("the page ended the feed at record %d: %v", index, cause)
		default:
			h.t.Fatal(err)
		}
	}
}

func (h *floodHarness) count(kind string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.events[kind]
}

func (h *floodHarness) drawn() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.next
}

// answerScreens plays the machine: each pending screen request gets a checkpoint at the current boundary.
func (h *floodHarness) answerScreens(boundary func() uint64) {
	h.connection.mu.Lock()
	pending := h.connection.screenRequestID != ""
	h.connection.mu.Unlock()
	if pending {
		b := boundary()
		h.connection.finishResync([]byte("screen"), b)
		h.mu.Lock()
		h.screen = b
		h.mu.Unlock()
	}
}

// A flood with drops keeps the page live: every gap ends in a fresh screen ("attached") and output resumes after it.
func TestFloodWithDropsKeepsDrawing(t *testing.T) {
	h := newFloodHarness(t)
	line := bytes.Repeat([]byte("1234567\n"), 90) // 720 bytes, like a seq record
	var sequence, index uint64
	deadline := time.Now().Add(20 * time.Second)
	for round := 0; round < 200 && time.Now().Before(deadline); round++ {
		for i := 0; i < 50; i++ {
			index++
			if round%7 == 3 && i == 10 {
				sequence += uint64(len(line)) // the edge dropped this one
				continue
			}
			h.send(index, sequence, line)
			sequence += uint64(len(line))
		}
		end := sequence
		h.answerScreens(func() uint64 { return end })
		time.Sleep(time.Millisecond)
	}
	final := sequence
	for time.Now().Before(deadline) {
		h.answerScreens(func() uint64 { return final })
		h.mu.Lock()
		shown := max(h.next, h.screen)
		h.mu.Unlock()
		if shown >= final {
			t.Logf("drew to the end: %v", h.events)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stopped drawing: drew to %d of %d; events %v", h.drawn(), final, h.events)
}

// The machine drops a backlog without an index gap (records stay numbered, the sequence jumps), the edge sends lag
// markers, screens arrive while the flood goes on and some come unasked: the page still ends on the final output.
func TestFloodWithHostAndEdgeDropsKeepsDrawing(t *testing.T) {
	h := newFloodHarness(t)
	line := bytes.Repeat([]byte("1234567\n"), 90)
	var sequence, index uint64
	deadline := time.Now().Add(20 * time.Second)
	for round := 0; round < 300 && time.Now().Before(deadline); round++ {
		for i := 0; i < 40; i++ {
			index++
			if round%5 == 2 && i == 5 {
				sequence += 64 << 10 // the machine dropped a backlog: same index run, later sequence
			}
			h.send(index, sequence, line)
			sequence += uint64(len(line))
		}
		if round%9 == 4 {
			h.ws.broadcasts.markLagged() // the edge dropped records for this browser
		}
		end := sequence
		if round%11 == 6 {
			h.connection.finishResync([]byte("unasked"), end) // a screen the machine sent on its own
		}
		h.answerScreens(func() uint64 { return end })
		time.Sleep(time.Millisecond)
	}
	final := sequence
	for time.Now().Before(deadline) {
		h.answerScreens(func() uint64 { return final })
		h.mu.Lock()
		shown := max(h.next, h.screen)
		h.mu.Unlock()
		if shown >= final {
			t.Logf("drew to the end: %v", h.events)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.connection.mu.Lock()
	request := h.connection.screenRequestID
	h.connection.mu.Unlock()
	h.ws.broadcasts.mu.Lock()
	queued, queuedBytes := len(h.ws.broadcasts.items), h.ws.broadcasts.bytes
	h.ws.broadcasts.mu.Unlock()
	t.Fatalf("stopped drawing: drew to %d of %d, screen %d; events %v; request %q, queued %d (%d bytes)", h.drawn(), final, h.screen, h.events, request, queued, queuedBytes)
}

// The page's events leave the worker on JS timers (output-flow.js schedules each batch with setTimeout): a flood must
// never keep the JS event loop from running, or every later event waits forever.
func TestFloodLeavesTheEventLoopRunning(t *testing.T) {
	h := newFloodHarness(t)
	var ticks int
	var mu sync.Mutex
	tick := js.FuncOf(func(js.Value, []js.Value) any { mu.Lock(); ticks++; mu.Unlock(); return nil })
	defer tick.Release()
	interval := js.Global().Call("setInterval", tick, 20)
	defer js.Global().Call("clearInterval", interval)
	line := bytes.Repeat([]byte("1234567\n"), 90)
	var sequence uint64
	records := make([][]byte, 5000) // sealed first: on a page the records arrive from JS, which costs this loop nothing
	for i := range records {
		if i%250 == 105 {
			sequence += 64 << 10
		}
		records[i] = h.seal(uint64(i+1), sequence, line)
		sequence += uint64(len(line))
	}
	started := time.Now()
	for round := 0; round < 100; round++ {
		for i := 0; i < 50; i++ {
			index := uint64(round*50 + i + 1)
			h.offer(index, records[index-1])
		}
		end := sequence
		h.answerScreens(func() uint64 { return end })
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(started)
	mu.Lock()
	got := ticks
	mu.Unlock()
	t.Logf("%d JS timer ticks in %v (%d expected at 20 ms)", got, elapsed, elapsed/(20*time.Millisecond))
	if got < int(elapsed/(20*time.Millisecond))/4 {
		t.Fatalf("the JS event loop starved: %d ticks in %v", got, elapsed)
	}
}
