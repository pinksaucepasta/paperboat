package execprocess

import (
	"context"
	"io"
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
