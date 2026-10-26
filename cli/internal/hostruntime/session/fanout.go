package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
)

var (
	ErrAttachmentExists  = errors.New("attachment already exists")
	ErrAttachmentUnknown = errors.New("attachment not found")
	ErrAttachmentEvicted = errors.New("slow consumer evicted")
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
	notify       chan struct{}
}

// Fanout retains one ordered ring of immutable chunks. Attachments track only
// sequence cursors and byte counts; they never own per-client event slices.
type Fanout struct {
	mu          sync.Mutex
	attachments map[string]*attachmentCursor
	// ringEnd is the largest EndSequence of the retained ring (0 when empty).
	ringEnd uint64
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
	f.attachments[attachmentID] = &attachmentCursor{state: Attached, receiveOutput: receiveOutput, backpressure: backpressure, progressAt: time.Now(), maxBytes: maxPendingBytes, notify: make(chan struct{}, 1)}
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
	var evictions []Eviction
	for id, cursor := range f.attachments {
		if cursor.state != Attached || !cursor.receiveOutput {
			continue
		}
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
