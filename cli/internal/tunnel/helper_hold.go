package tunnel

import (
	"sync"
	"sync/atomic"
	"time"
)

// Ctrl-C hold (F3-11). A lone Ctrl-C leaves megabytes of output already on its way (compressed 100:1 it fits the
// transport buffers), which the console then paints at a few hundred KB/s long after the program is gone. From the
// moment the key is sent the daemon holds incoming terminal output instead of passing it on, still reading at full
// speed, until the host answers the key (terminal_input_result carries interrupt_through and interrupt_discarded):
//   - discarded: the host dropped this viewer's queued output up to through and sends a screen instead. The held output
//     up to through is dropped; later arrivals up to through are dropped too (they were already in flight). The screen
//     (channel 3) is never held.
//   - not discarded (idle prompt, small backlog, the key was data in raw mode): the held output is released unchanged,
//     nothing repainted, scrollback kept.
//   - no answer within helperHoldTimeout, a refused key, a full hold buffer or the end of the stream: released unchanged.
//
// Eternal Terminal does the same with its own queues on an interrupt (client filterDroppable, TerminalClient.cpp, flush
// from 64 KiB, WriteBuffer.hpp); here the host decides what was discarded and says so.
// helperHoldTimeout is a variable so tests can shorten it.
var helperHoldTimeout = 3 * time.Second

const helperHoldMaxBytes = 8 << 20

type helperHold struct {
	mu        sync.Mutex
	active    bool
	etx       uint64
	buffered  []helperOutput
	bytes     int
	timer     *time.Timer
	dropBelow atomic.Uint64 // after a discarded answer: output that ends at or before this is dropped on arrival
	readDrop  atomic.Uint64 // the same cut for output already queued for Read: ends at the first queued chunk past it, which the arrivals above never see
}

// sendOut passes one output on to Read; false means the connection is done.
func (c *helperTerminalConn) sendOut(output helperOutput) bool {
	select {
	case c.out <- output:
		return true
	case <-c.done:
		return false
	}
}

// startHold begins (or extends) the hold for the Ctrl-C that has input sequence etx.
func (c *helperTerminalConn) startHold(etx uint64) {
	h := &c.hold
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active, h.etx = true, etx
	h.dropBelow.Store(0)
	h.readDrop.Store(0)
	if h.timer != nil {
		h.timer.Stop()
	}
	h.timer = time.AfterFunc(helperHoldTimeout, func() { c.releaseHold(etx) })
}

// routeOutput is where the read loop hands every decoded output: held, dropped or passed on.
func (c *helperTerminalConn) routeOutput(output helperOutput) bool {
	h := &c.hold
	h.mu.Lock()
	defer h.mu.Unlock()
	if output.checkpoint || !output.byteCursor {
		return c.sendOut(output)
	}
	if h.active {
		if h.bytes+len(output.data) > helperHoldMaxBytes {
			return c.releaseLocked(output)
		}
		h.buffered = append(h.buffered, output)
		h.bytes += len(output.data)
		return true
	}
	if limit := h.dropBelow.Load(); limit != 0 {
		var keep bool
		if output, keep = trimBelow(output, limit); !keep {
			return true
		}
		h.dropBelow.Store(0)
	}
	return c.sendOut(output)
}

// trimBelow drops the part of output that ends at or before limit; keep is false when nothing is left.
func trimBelow(output helperOutput, limit uint64) (helperOutput, bool) {
	start := output.endSequence - uint64(len(output.data))
	switch {
	case output.endSequence <= limit:
		return output, false
	case start < limit:
		output.data = output.data[limit-start:]
	}
	return output, true
}

// releaseLocked passes everything held on, then extra (when it is not the zero value), in order.
func (c *helperTerminalConn) releaseLocked(extra ...helperOutput) bool {
	h := &c.hold
	held := h.buffered
	h.active, h.buffered, h.bytes = false, nil, 0
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	for _, output := range held {
		if !c.sendOut(output) {
			return false
		}
	}
	for _, output := range extra {
		if !c.sendOut(output) {
			return false
		}
	}
	return true
}

// releaseHold is the timeout: whatever was held goes on unchanged.
func (c *helperTerminalConn) releaseHold(etx uint64) {
	h := &c.hold
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active && h.etx == etx {
		c.releaseLocked()
	}
}

// flushHold releases what is held when the stream ends: scrollback is never stranded in the hold.
func (c *helperTerminalConn) flushHold() {
	h := &c.hold
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active {
		c.releaseLocked()
	}
}

// resolveHold is the host's answer to the Ctrl-C with input sequence etx. An answer for another key is ignored.
func (c *helperTerminalConn) resolveHold(etx, through uint64, discarded bool) {
	h := &c.hold
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active || h.etx != etx {
		return
	}
	if !discarded {
		c.releaseLocked()
		return
	}
	held := h.buffered
	h.active, h.buffered, h.bytes = false, nil, 0
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.dropBelow.Store(through)
	h.readDrop.Store(through)
	for _, output := range held {
		if kept, ok := trimBelow(output, through); ok {
			h.dropBelow.Store(0)
			if !c.sendOut(kept) {
				return
			}
		}
	}
}

// skipDropped applies a discarded Ctrl-C's cut to output that was already queued for Read when the answer came: a flood
// leaves up to helperOutputQueueDecodedLimit (16 MiB) there, and painting it takes as long as the flood took to print
// (measured X-24: 19 MB for 40 s after Ctrl-C). It takes no lock: routeOutput holds the hold's mutex while it waits
// for Read to take from the same queue.
func (c *helperTerminalConn) skipDropped(output helperOutput) (helperOutput, bool) {
	limit := c.hold.readDrop.Load()
	if limit == 0 || output.checkpoint || !output.byteCursor {
		return output, true
	}
	kept, ok := trimBelow(output, limit)
	if ok {
		c.hold.readDrop.CompareAndSwap(limit, 0)
	}
	return kept, ok
}
