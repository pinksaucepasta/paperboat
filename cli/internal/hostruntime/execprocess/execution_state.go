package execprocess

import (
	"context"
	"io"
	"sync"
	"time"
)

func (e *Execution) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

func (e *Execution) snapshotLocked() Snapshot {
	value := Snapshot{OperationID: e.request.OperationID, State: e.state, StartedAt: e.startedAt, ErrorCode: e.errorCode, NextSequence: e.next}
	if e.result != nil {
		result := *e.result
		value.Result = &result
	}
	return value
}

// WaitStarted publishes running or terminal state only after the launch decision.
// Start remains asynchronous; an application attachment must await this owner
// before advertising a writable input stream.
func (e *Execution) WaitStarted(ctx context.Context) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, ErrInvalid
	}
	for {
		e.mu.Lock()
		snapshot, wake := e.snapshotLocked(), e.wake
		e.mu.Unlock()
		if snapshot.State != StateAuthorized && snapshot.State != StateStarting {
			return snapshot, nil
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
}

func (e *Execution) Wait(ctx context.Context) (Snapshot, error) {
	select {
	case <-e.done:
		e.mu.Lock()
		err := e.waitErr
		e.mu.Unlock()
		return e.Snapshot(), err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

func (e *Execution) Next(ctx context.Context, from uint64) (Event, error) {
	for {
		e.mu.Lock()
		if from < e.earliest {
			e.mu.Unlock()
			return Event{}, ErrReplayUnavailable
		}
		if from < e.next {
			event := e.events[from-e.earliest]
			e.mu.Unlock()
			return cloneEvent(event), nil
		}
		wake := e.wake
		terminal := terminalState(e.state)
		e.mu.Unlock()
		if terminal {
			return Event{}, io.EOF
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// OpenReader registers a live delivery cursor. Replay remains bounded when no
// reader is attached; an attached reader pins unread events and backpressures
// the child process instead of allowing ordinary live output to become a gap.
func (e *Execution) OpenReader(from uint64) (ReaderService, error) {
	if e == nil || from == 0 {
		return nil, ErrInvalid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if from < e.earliest {
		return nil, ErrReplayUnavailable
	}
	e.nextReader++
	if e.nextReader == 0 {
		return nil, ErrCapacity
	}
	id := e.nextReader
	e.readers[id] = from
	if e.linger != nil {
		e.linger.Stop()
		e.linger = nil
	}
	return &Reader{execution: e, id: id, next: from}, nil
}

func (r *Reader) Next(ctx context.Context) (Event, func(), error) {
	if r == nil || r.execution == nil || ctx == nil {
		return Event{}, nil, ErrInvalid
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Event{}, nil, io.EOF
	}
	from := r.next
	r.mu.Unlock()
	event, err := r.execution.Next(ctx, from)
	if err != nil {
		return Event{}, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			r.mu.Lock()
			if !r.closed && r.next == event.Sequence {
				r.next = event.Sequence + 1
			}
			next, closed := r.next, r.closed
			r.mu.Unlock()
			if !closed {
				r.execution.advanceReader(r.id, next)
			}
		})
	}
	return event, release, nil
}

func (r *Reader) Close() error {
	if r == nil || r.execution == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	r.execution.removeReader(r.id)
	return nil
}

func (e *Execution) advanceReader(id, next uint64) {
	e.mu.Lock()
	if _, ok := e.readers[id]; ok {
		e.readers[id] = next
		e.delivered = max(e.delivered, next)
		e.evictLocked()
		e.signalLocked()
	}
	e.mu.Unlock()
}

func (e *Execution) removeReader(id uint64) {
	e.mu.Lock()
	delete(e.readers, id)
	e.evictLocked()
	e.signalLocked()
	if len(e.readers) == 0 && !terminalState(e.state) && e.linger == nil {
		e.linger = time.AfterFunc(e.manager.config.DetachedLinger, e.abandon)
	}
	e.mu.Unlock()
}

// abandon ends a command no client has been attached to for DetachedLinger,
// like Teleport's lingerAndDie (lib/srv/sess.go) for a party-less session and
// Tailscale's killProcessOnContextDone (ssh/tailssh/tailssh.go) once the
// channel is gone: SIGTERM, then SIGKILL after CancelGrace. A reattach within
// the linger stops the timer.
func (e *Execution) abandon() {
	e.mu.Lock()
	e.linger = nil
	abandoned := len(e.readers) == 0 && !terminalState(e.state)
	e.mu.Unlock()
	if !abandoned {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.manager.config.CancelGrace+5*time.Second)
	defer cancel()
	reportExecutionFailure(ctx, "lifecycle", "exec_operation_failed", e.Cancel(ctx))
}
