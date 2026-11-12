package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
)

var (
	ErrAttachmentExists  = errors.New("attachment already exists")
	ErrAttachmentUnknown = errors.New("attachment not found")
	// ErrAttachmentReplaced: the same client attached this attachment again (a resume or a reattach took it over),
	// so the stream holding the earlier lease ends and the newer one has the terminal.
	ErrAttachmentReplaced = errors.New("attachment taken over by a newer attach")
	ErrAttachmentEvicted  = errors.New("slow consumer evicted")
	// ErrAttachmentLagged: a free-running attachment fell behind and its backlog was dropped; the consumer
	// restores a fresh screen in-band and keeps its connection.
	ErrAttachmentLagged  = errors.New("attachment fell behind; restore the screen")
	ErrOutputOrder       = errors.New("output sequence is not contiguous")
	ErrInvalidQueueLimit = errors.New("invalid attachment queue limit")
)

type Eviction struct {
	AttachmentID string
	QueuedBytes  uint64
	Reason       string
}

type attachmentCursor struct {
	lease         uint64
	state         AttachmentState
	receiveOutput bool
	// backpressure: the controlling owner, whose queue holds the PTY back (WaitRoom) so it sees every byte. Any
	// other attachment runs free and lags (backlog dropped, ErrAttachmentLagged) instead of slowing the program.
	backpressure bool
	owner        bool // may become the controller (SetController)
	lagged       bool
	progressAt   time.Time
	maxBytes     uint64
	queuedBytes  uint64
	evictedBytes uint64
	cursor       uint64
	enqueuedEnd  uint64
	hasCursor    bool
	rate         outputRate
	notify       chan struct{}
}

// Fanout retains one ordered ring of immutable chunks. Attachments track only
// sequence cursors and byte counts; they never own per-client event slices.
type Fanout struct {
	lastLease   uint64
	mu          sync.Mutex
	attachments map[string]*attachmentCursor
	// ringEnd is the largest EndSequence of the retained ring (0 when empty).
	ringEnd uint64
	// published is the end of the last event published, never reset: the boundary an interrupt cuts to.
	published uint64
	// minimum, haveMinimum and atMinimum are the attached cursors' minimum at the last compaction and how
	// many attached cursors were on it.
	minimum     uint64
	haveMinimum bool
	atMinimum   int
	active      sync.Map
	ring        []history.Event
	// room wakes the producer waiting in WaitRoom when a consumer drains.
	room chan struct{}
}

// outputRate is how much a viewer was sent over about the last second (an exponential window).
type outputRate struct {
	bytes float64
	at    time.Time
}

func (r *outputRate) now(at time.Time) float64 {
	if r.at.IsZero() || at.Before(r.at) {
		return r.bytes
	}
	return r.bytes * math.Exp(-float64(at.Sub(r.at))/float64(time.Second))
}

func (r *outputRate) add(at time.Time, size int) { r.bytes, r.at = r.now(at)+float64(size), at }

func NewFanout() *Fanout {
	return &Fanout{attachments: make(map[string]*attachmentCursor), room: make(chan struct{}, 1)}
}

func (f *Fanout) Attach(attachmentID string, maxPendingBytes uint64) error {
	return f.attach(attachmentID, maxPendingBytes, true, false)
}

// AttachBackpressured attaches a window that may pace the PTY (a native owner or interactive terminal). It paces
// only while the size rule names it the controller (SetController); until then it runs free like any viewer.
func (f *Fanout) AttachBackpressured(attachmentID string, maxPendingBytes uint64) error {
	if err := f.attach(attachmentID, maxPendingBytes, true, false); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachments[attachmentID].owner = true
	return nil
}

// SetController makes attachmentID the one attachment that paces the PTY. The controller of the size rule may be a
// window that cannot pace (a browser, or none attached): then nothing paces and every attachment runs free.
func (f *Fanout) SetController(attachmentID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.attachments[attachmentID]
	if target != nil && target.backpressure {
		return
	}
	for _, cursor := range f.attachments {
		cursor.backpressure = false
	}
	if target != nil && target.state == Attached && target.owner {
		target.backpressure, target.progressAt = true, time.Now()
	}
	notify(f.room)
}

// WaitRoom holds the producer until every backpressured attachment has room for size more bytes. An owner
// that drains nothing for stall is treated as gone (a dead link): WaitRoom returns and the next publish evicts
// it, so a suspended laptop never freezes the program; it reconnects and replays from history.
func (f *Fanout) WaitRoom(ctx context.Context, size uint64, stall time.Duration) {
	for {
		f.mu.Lock()
		full := false
		for _, cursor := range f.attachments {
			if cursor.state == Attached && cursor.backpressure && size > cursor.maxBytes-cursor.queuedBytes && time.Since(cursor.progressAt) < stall {
				full = true
				break
			}
		}
		f.mu.Unlock()
		if !full {
			return
		}
		timer := time.NewTimer(stall / 10)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-f.room:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// AttachControl keeps an authorized participant present for input and
// revocation while a shared encrypted output feed serves the browser.
func (f *Fanout) AttachControl(attachmentID string, maxPendingBytes uint64) error {
	return f.attach(attachmentID, maxPendingBytes, false, false)
}

func (f *Fanout) attach(attachmentID string, maxPendingBytes uint64, receiveOutput, backpressure bool) error {
	if attachmentID == "" || maxPendingBytes == 0 {
		return ErrInvalidQueueLimit
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing := f.attachments[attachmentID]; existing != nil && existing.state == Attached {
		return ErrAttachmentExists
	}
	if f.lastLease == ^uint64(0) {
		return ErrResourceLimit
	}
	f.lastLease++
	f.attachments[attachmentID] = &attachmentCursor{lease: f.lastLease, state: Attached, receiveOutput: receiveOutput, backpressure: backpressure, progressAt: time.Now(), maxBytes: maxPendingBytes, notify: make(chan struct{}, 1)}
	f.active.Store(attachmentID, struct{}{})
	return nil
}

func (f *Fanout) Detach(attachmentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor, ok := f.attachments[attachmentID]
	if !ok {
		return ErrAttachmentUnknown
	}
	if cursor.state != Attached && cursor.state != Evicted {
		return ErrInvalidTransition
	}
	cursor.state = Detached
	f.active.Delete(attachmentID)
	notify(cursor.notify)
	delete(f.attachments, attachmentID)
	f.compactLocked()
	return nil
}

func (f *Fanout) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, cursor := range f.attachments {
		if cursor.state == Attached {
			count++
		}
	}
	return count
}

// IsAttached is lock-free so terminal input never waits behind output fanout.
func (f *Fanout) IsAttached(attachmentID string) bool {
	_, ok := f.active.Load(attachmentID)
	return ok
}

// SkipTo discards output covered by a screen checkpoint before the stream
// starts. The session lock makes the checkpoint and this cursor update atomic
// with PTY output publication.
func (f *Fanout) SkipTo(attachmentID string, nextSequence uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor, ok := f.attachments[attachmentID]
	if !ok || cursor.state != Attached {
		return ErrAttachmentUnknown
	}
	if !cursor.receiveOutput {
		// A control-only attachment (a browser fed by the shared publisher) queues nothing, so its end never
		// advances: every screen moves it to the current boundary (repeated screens under a flood, F3-13).
		cursor.cursor, cursor.enqueuedEnd, cursor.hasCursor, cursor.queuedBytes = nextSequence, nextSequence, true, 0
		f.compactLocked()
		return nil
	}
	if cursor.hasCursor && (nextSequence < cursor.cursor || nextSequence > cursor.enqueuedEnd) {
		return ErrOutputOrder
	}
	previous, moved := cursor.cursor, cursor.hasCursor
	if !cursor.hasCursor {
		cursor.enqueuedEnd = nextSequence
		cursor.hasCursor = true
	}
	cursor.cursor = nextSequence
	cursor.queuedBytes = cursor.enqueuedEnd - nextSequence
	if moved {
		f.compactAfterAdvanceLocked(previous)
	} else {
		f.compactLocked()
	}
	return nil
}

// PublishedEnd is the end of the last output published: what an interrupt handled now has already been queued.
func (f *Fanout) PublishedEnd() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published
}

// interruptDiscardThreshold is the backlog below which Ctrl-C leaves a viewer's queue alone: a prompt-sized tail
// drains at once, only a flood is worth a resync (EternalTerminal flushes on interrupt from 64 KiB,
// WriteBuffer.hpp FLUSH_THRESHOLD).
const interruptDiscardThreshold = 64 << 10

// DiscardPendingOutput drops the terminal output queued for every attached viewer up to through, the PublishedEnd
// taken when the interrupt arrived: the userspace counterpart of a tty flushing its output queue on VINTR. A viewer
// whose backlog reaches interruptDiscardThreshold jumps to through and is marked lagged, the state a free runner gets
// when it falls behind a flood: its stream then sends it the current screen in-band (dispatcher ErrAttachmentLagged),
// which already holds everything the program printed after the interrupt, the shell's prompt above all, so the
// emulator is never left mid-escape-sequence by the gap. History keeps the flood for replay. A viewer with a resize
// (control event) among its pending output keeps its whole backlog: the screen resync carries no size change, and
// skipping the event would leave that viewer at the wrong size. It returns the attachments it resynced.
func (f *Fanout) DiscardPendingOutput(through uint64) (resynced []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for id, cursor := range f.attachments {
		if cursor.state != Attached || !cursor.receiveOutput || !cursor.hasCursor {
			continue
		}
		target := min(through, cursor.enqueuedEnd)
		lag := uint64(0)
		if target > cursor.cursor {
			lag = target - cursor.cursor
		}
		// A flood leaves megabytes in the transport that the cursor has already passed, so the queue here can be empty
		// while the viewer is minutes behind (measured X-24: 19 MB painted after Ctrl-C): the recent rate decides too.
		flood := cursor.rate.now(now) >= interruptDiscardThreshold/2 // half: the window decays between the last chunk and the key
		if lag < interruptDiscardThreshold && !flood || f.hasControlLocked(cursor.cursor, target) {
			continue
		}
		if lag > 0 {
			cursor.cursor, cursor.queuedBytes = target, cursor.enqueuedEnd-target
		}
		cursor.lagged = true
		notify(cursor.notify)
		resynced = append(resynced, id)
	}
	f.compactLocked()
	notify(f.room)
	return resynced
}

// Lagged reports an attachment whose backlog was dropped and whose screen has not been sent yet.
func (f *Fanout) Lagged(attachmentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor, ok := f.attachments[attachmentID]
	return ok && cursor.lagged
}

// hasControlLocked reports a non-terminal-output (resize) event queued in [from, to).
func (f *Fanout) hasControlLocked(from, to uint64) bool {
	for _, event := range f.ring {
		if event.Channel != 1 && event.StartSequence >= from && event.StartSequence < to {
			return true
		}
	}
	return false
}

func (f *Fanout) Publish(event history.Event) ([]Eviction, error) {
	event.Data = append([]byte(nil), event.Data...)
	return f.PublishOwned(event)
}

func (f *Fanout) PublishOwned(event history.Event) ([]Eviction, error) {
	if !validOutputEvent(event) {
		return nil, ErrOutputOrder
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, cursor := range f.attachments {
		if cursor.state == Attached && cursor.receiveOutput && cursor.hasCursor && cursor.enqueuedEnd != event.StartSequence {
			return nil, fmt.Errorf("attachment %s expected %d, got %d: %w", id, cursor.enqueuedEnd, event.StartSequence, ErrOutputOrder)
		}
	}
	f.insertEventLocked(event)
	f.published = event.EndSequence
	now := time.Now()
	var evictions []Eviction
	for id, cursor := range f.attachments {
		if cursor.state != Attached || !cursor.receiveOutput {
			continue
		}
		cursor.rate.add(now, len(event.Data)) // before the overflow branch: a viewer that lags under a flood is the one that counts
		if uint64(len(event.Data)) > cursor.maxBytes-cursor.queuedBytes {
			if !cursor.backpressure {
				// Drop the backlog, this event included: the consumer restores a screen covering all of it.
				cursor.cursor, cursor.enqueuedEnd, cursor.hasCursor = event.EndSequence, event.EndSequence, true
				cursor.queuedBytes, cursor.lagged = 0, true
				notify(cursor.notify)
				continue
			}
			evictions = append(evictions, Eviction{AttachmentID: id, QueuedBytes: cursor.queuedBytes, Reason: "slow_consumer"})
			cursor.evictedBytes = cursor.queuedBytes
			cursor.queuedBytes = 0
			cursor.state = Evicted
			f.active.Delete(id)
			notify(cursor.notify)
			continue
		}
		if !cursor.hasCursor {
			cursor.cursor = event.StartSequence
			cursor.hasCursor = true
		}
		cursor.enqueuedEnd = event.EndSequence
		cursor.queuedBytes += uint64(len(event.Data))
		notify(cursor.notify)
	}
	f.compactLocked()
	return evictions, nil
}

func (f *Fanout) Enqueue(attachmentID string, event history.Event) (*Eviction, error) {
	event.Data = append([]byte(nil), event.Data...)
	return f.EnqueueOwned(attachmentID, event)
}

func (f *Fanout) EnqueueOwned(attachmentID string, event history.Event) (*Eviction, error) {
	if !validOutputEvent(event) {
		return nil, ErrOutputOrder
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor, ok := f.attachments[attachmentID]
	if !ok {
		return nil, ErrAttachmentUnknown
	}
	if cursor.state == Evicted {
		return nil, ErrAttachmentEvicted
	}
	if cursor.state != Attached {
		return nil, ErrInvalidTransition
	}
	if cursor.hasCursor && cursor.enqueuedEnd != event.StartSequence {
		return nil, ErrOutputOrder
	}
	if uint64(len(event.Data)) > cursor.maxBytes-cursor.queuedBytes {
		eviction := &Eviction{AttachmentID: attachmentID, QueuedBytes: cursor.queuedBytes, Reason: "slow_consumer"}
		cursor.evictedBytes = cursor.queuedBytes
		cursor.queuedBytes = 0
		cursor.state = Evicted
		f.active.Delete(attachmentID)
		notify(cursor.notify)
		f.compactLocked()
		return eviction, nil
	}
	f.insertEventLocked(event)
	if !cursor.hasCursor {
		cursor.cursor = event.StartSequence
		cursor.hasCursor = true
	}
	cursor.enqueuedEnd = event.EndSequence
	cursor.queuedBytes += uint64(len(event.Data))
	notify(cursor.notify)
	return nil, nil
}

func (f *Fanout) WaitNext(ctx context.Context, attachmentID string) (history.Event, error) {
	event, err := f.WaitNextOwned(ctx, attachmentID)
	if err != nil {
		return history.Event{}, err
	}
	data := append([]byte(nil), event.Data...)
	event.Release()
	event.Data = data
	return event, nil
}

// WaitNextFrom returns the next output event at or after from without consuming it. from confirms that the caller
// holds everything before it, which frees that output; a caller whose answer was lost in transit (a poll deadline
// racing the reply) asks again from the same place and gets the same event, so nothing is skipped.
func (f *Fanout) WaitNextFrom(ctx context.Context, attachmentID string, from uint64) (history.Event, error) {
	return f.waitNextFromLease(ctx, attachmentID, from, 0)
}

func (f *Fanout) waitNextFromLease(ctx context.Context, attachmentID string, from, lease uint64) (history.Event, error) {
	for {
		f.mu.Lock()
		cursor, ok := f.attachments[attachmentID]
		if !ok {
			f.mu.Unlock()
			return history.Event{}, ErrAttachmentUnknown
		}
		if lease != 0 && cursor.lease != lease {
			f.mu.Unlock()
			return history.Event{}, ErrAttachmentReplaced
		}
		if cursor.state == Evicted {
			f.mu.Unlock()
			return history.Event{}, ErrAttachmentEvicted
		}
		if cursor.state != Attached {
			f.mu.Unlock()
			return history.Event{}, ErrInvalidTransition
		}
		if cursor.lagged {
			cursor.lagged = false
			f.mu.Unlock()
			return history.Event{}, ErrAttachmentLagged
		}
		if cursor.hasCursor && from > cursor.cursor && from <= cursor.enqueuedEnd {
			previous := cursor.cursor
			cursor.queuedBytes -= from - previous
			cursor.cursor, cursor.progressAt = from, time.Now()
			f.compactAfterAdvanceLocked(previous)
			notify(f.room)
		}
		if cursor.hasCursor && cursor.cursor < cursor.enqueuedEnd {
			event, found := f.eventAtLocked(cursor.cursor)
			if !found {
				f.mu.Unlock()
				return history.Event{}, ErrOutputOrder
			}
			retained := event.Retain()
			offset := cursor.cursor - event.StartSequence
			retained.StartSequence = cursor.cursor
			retained.Data = event.Data[offset:]
			f.mu.Unlock()
			return retained, nil
		}
		notification := cursor.notify
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return history.Event{}, ctx.Err()
		case <-notification:
		}
	}
}

func (f *Fanout) WaitNextOwned(ctx context.Context, attachmentID string) (history.Event, error) {
	for {
		f.mu.Lock()
		cursor, ok := f.attachments[attachmentID]
		if !ok {
			f.mu.Unlock()
			return history.Event{}, ErrAttachmentUnknown
		}
		if cursor.state == Evicted {
			f.mu.Unlock()
			return history.Event{}, ErrAttachmentEvicted
		}
		if cursor.state != Attached {
			f.mu.Unlock()
			return history.Event{}, ErrInvalidTransition
		}
		if cursor.lagged {
			cursor.lagged = false
			f.mu.Unlock()
			return history.Event{}, ErrAttachmentLagged
		}
		if cursor.hasCursor && cursor.cursor < cursor.enqueuedEnd {
			event, found := f.eventAtLocked(cursor.cursor)
			if !found {
				f.mu.Unlock()
				return history.Event{}, ErrOutputOrder
			}
			start := cursor.cursor
			retained := event.Retain()
			offset := start - event.StartSequence
			retained.StartSequence = start
			retained.Data = event.Data[offset:]
			retained.EndSequence = event.EndSequence
			consumed := retained.EndSequence - retained.StartSequence
			cursor.cursor = retained.EndSequence
			cursor.queuedBytes -= consumed
			cursor.progressAt = time.Now()
			f.compactAfterAdvanceLocked(start)
			f.mu.Unlock()
			notify(f.room)
			return retained, nil
		}
		notification := cursor.notify
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return history.Event{}, ctx.Err()
		case <-notification:
		}
	}
}

func (f *Fanout) Next(attachmentID string) (history.Event, bool, error) {
	f.mu.Lock()
	cursor, ok := f.attachments[attachmentID]
	if !ok {
		f.mu.Unlock()
		return history.Event{}, false, ErrAttachmentUnknown
	}
	if cursor.state == Evicted {
		f.mu.Unlock()
		return history.Event{}, false, ErrAttachmentEvicted
	}
	if cursor.state != Attached {
		f.mu.Unlock()
		return history.Event{}, false, ErrInvalidTransition
	}
	if cursor.lagged {
		cursor.lagged = false
		f.mu.Unlock()
		return history.Event{}, false, ErrAttachmentLagged
	}
	if !cursor.hasCursor || cursor.cursor >= cursor.enqueuedEnd {
		f.mu.Unlock()
		return history.Event{}, false, nil
	}
	event, found := f.eventAtLocked(cursor.cursor)
	if !found {
		f.mu.Unlock()
		return history.Event{}, false, ErrOutputOrder
	}
	start := cursor.cursor
	data := append([]byte(nil), event.Data[start-event.StartSequence:]...)
	end := event.EndSequence
	cursor.cursor = end
	cursor.queuedBytes -= end - start
	cursor.progressAt = time.Now()
	f.compactAfterAdvanceLocked(start)
	f.mu.Unlock()
	notify(f.room)
	return history.Event{Channel: event.Channel, StartSequence: start, EndSequence: end, Data: data}, true, nil
}

func (f *Fanout) Status(attachmentID string) (AttachmentState, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor, ok := f.attachments[attachmentID]
	if !ok {
		return "", 0, ErrAttachmentUnknown
	}
	queued := cursor.queuedBytes
	if cursor.state == Evicted {
		queued = cursor.evictedBytes
	}
	return cursor.state, queued, nil
}

func validOutputEvent(event history.Event) bool {
	return event.EndSequence >= event.StartSequence && event.EndSequence-event.StartSequence == uint64(len(event.Data)) && len(event.Data) != 0
}

func (f *Fanout) eventAtLocked(sequence uint64) (history.Event, bool) {
	index := sort.Search(len(f.ring), func(i int) bool { return f.ring[i].EndSequence > sequence })
	if index == len(f.ring) || f.ring[index].StartSequence > sequence {
		return history.Event{}, false
	}
	return f.ring[index], true
}

func (f *Fanout) insertEventLocked(event history.Event) {
	// Output arrives in order: an event that starts at or after every retained event's end is neither
	// a duplicate nor overlapping and keeps the ring sorted, so it is appended without a scan or a
	// re-sort (each chunk used to cost a full sort of the ring).
	if event.StartSequence >= f.ringEnd {
		f.ring = append(f.ring, event.Retain())
		f.ringEnd = event.EndSequence
		return
	}
	for index, existing := range f.ring {
		if existing.StartSequence <= event.StartSequence && existing.EndSequence >= event.EndSequence && existing.Channel == event.Channel && bytes.Equal(existing.Data[event.StartSequence-existing.StartSequence:event.EndSequence-existing.StartSequence], event.Data) {
			return
		}
		if event.StartSequence <= existing.StartSequence && event.EndSequence >= existing.EndSequence && event.Channel == existing.Channel && bytes.Equal(event.Data[existing.StartSequence-event.StartSequence:existing.EndSequence-event.StartSequence], existing.Data) {
			existing.Release()
			f.ring = append(f.ring[:index], f.ring[index+1:]...)
			f.insertEventLocked(event)
			return
		}
	}
	f.ring = append(f.ring, event.Retain())
	f.ringEnd = max(f.ringEnd, event.EndSequence)
	sort.Slice(f.ring, func(i, j int) bool {
		if f.ring[i].StartSequence == f.ring[j].StartSequence {
			return f.ring[i].EndSequence > f.ring[j].EndSequence
		}
		return f.ring[i].StartSequence < f.ring[j].StartSequence
	})
}

// Release drops the retained output of a stopped session that no attached viewer still needs. No later
// output can arrive and a stopped terminal offers none of its old output to a new attach, but a viewer
// that is attached still receives what it was owed (the shell's last output before it exited); that
// tail is freed as it is read or when the viewer detaches.
func (f *Fanout) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compactLocked()
}

// compactAfterAdvanceLocked compacts after one attachment's cursor moved on from previous, only when it
// was the last cursor on the attached minimum: a compaction scans every attachment, and doing it on every
// read made a shared terminal cost O(viewers²) per output chunk. Viewers reading in step trigger one scan
// per round; a single slow viewer frees output with each of its reads.
func (f *Fanout) compactAfterAdvanceLocked(previous uint64) {
	if !f.haveMinimum || previous != f.minimum {
		return
	}
	if f.atMinimum--; f.atMinimum > 0 {
		return
	}
	f.compactLocked()
}

func (f *Fanout) compactLocked() {
	var minimum uint64
	haveMinimum, atMinimum := false, 0
	for _, cursor := range f.attachments {
		if cursor.state != Attached || !cursor.hasCursor {
			continue
		}
		switch {
		case !haveMinimum || cursor.cursor < minimum:
			minimum, haveMinimum, atMinimum = cursor.cursor, true, 1
		case cursor.cursor == minimum:
			atMinimum++
		}
	}
	f.minimum, f.haveMinimum, f.atMinimum = minimum, haveMinimum, atMinimum
	if !haveMinimum {
		for _, event := range f.ring {
			event.Release()
		}
		f.ring, f.ringEnd = nil, 0
		return
	}
	index := 0
	for index < len(f.ring) && f.ring[index].EndSequence <= minimum {
		f.ring[index].Release()
		index++
	}
	if index > 0 {
		copy(f.ring, f.ring[index:])
		clear(f.ring[len(f.ring)-index:])
		f.ring = f.ring[:len(f.ring)-index]
		if len(f.ring) == 0 {
			f.ringEnd = 0
		}
	}
}

func notify(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}
