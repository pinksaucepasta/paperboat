// Package offlineaudit records the sessions a host starts or refuses while Paperboat is
// unreachable (F21-11) and uploads them on reconnect. Audit fails closed: an event is on disk
// before the session starts, and a full backlog refuses new offline sessions. It also queues the
// start, refusal and end of every managed SSH session, online or offline (F5-12), because SSH has
// no server-side admission; the host's upload cadence is bounded (Uploader.MinInterval).
package offlineaudit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
	wire "github.com/pinksaucepasta/paperboat/shared/offlineaudit"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

const (
	// MaxEvents and MaxAge bound the backlog (PLAN 3b "Offline-capable authority" point 5); the
	// server accepts events up to MaxAge old.
	MaxEvents = 10_000
	MaxAge    = 7 * 24 * time.Hour

	KindStarted = "offline_session_started"
	KindRefused = "offline_session_refused"
	// KindEnded is a peer-started exec's end: Outcome is its final state, with ExitCode and DurationMS.
	KindEnded = "offline_session_ended"

	KindSSHStarted = "ssh_session_started"
	KindSSHRefused = "ssh_session_refused"
	KindSSHEnded   = "ssh_session_ended"

	OriginOnline  = "online"
	OriginOffline = "offline"

	ReasonNoScope     = "no_scope"
	ReasonBacklogFull = "backlog_full"
	ReasonPeerUnknown = "peer_unknown"
)

// ErrFull refuses a new offline session: it needs Paperboat online.
var ErrFull = errors.New("the offline audit backlog is full; the session needs Paperboat online")

type Event = wire.Event

// Queue is the one owner of the backlog file.
type Queue struct {
	path    string
	now     func() time.Time
	mu      sync.Mutex
	events  []Event
	changed chan struct{}
	// Adopt holds the terminal sessions created while Paperboat was unreachable that the server has not been told
	// about yet (an event's OperationID is the session id, PeerAccountID its owner); the Adopter registers them once
	// Paperboat answers. It lives beside the backlog file so that every holder of the backlog also holds it.
	Adopt *Queue
}

func Open(path string, now func() time.Time) (*Queue, error) {
	q, err := open(path, now)
	if err != nil {
		return nil, err
	}
	if q.Adopt, err = open(path+".adopt", now); err != nil {
		return nil, err
	}
	return q, nil
}

func open(path string, now func() time.Time) (*Queue, error) {
	if now == nil {
		now = serverclock.Now
	}
	q := &Queue{path: path, now: now, changed: make(chan struct{}, 1)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the offline audit backlog: %w", err)
	}
	if err := json.Unmarshal(data, &q.events); err != nil || len(q.events) > MaxEvents {
		return nil, fmt.Errorf("the offline audit backlog %s is unreadable; remove it to continue: %w", path, errors.Join(err, errors.New("invalid backlog")))
	}
	if len(q.events) > 0 {
		q.changed <- struct{}{}
	}
	return q, nil
}

// Record stores one decision before the caller acts on it. A started session that cannot be
// recorded is refused (ErrFull, or the write error); a refusal is recorded when there is room.
// An event that names its own EventID keeps it, so a later end can refer to its start.
func (q *Queue) Record(event Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now().UTC()
	if len(q.events) >= MaxEvents || len(q.events) > 0 && now.Sub(q.events[0].OccurredAt) >= MaxAge {
		return ErrFull
	}
	if event.EventID == "" {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		event.EventID = id.String()
	}
	event.OccurredAt = now.Truncate(time.Second)
	next := append(q.events[:len(q.events):len(q.events)], event)
	if err := writeAtomic(q.path, next); err != nil {
		return fmt.Errorf("record the offline session for audit: %w", err)
	}
	q.events = next
	select {
	case q.changed <- struct{}{}:
	default:
	}
	return nil
}

// Changed signals a new event (and, once, a non-empty backlog at open).
func (q *Queue) Changed() <-chan struct{} { return q.changed }

// Oldest returns up to n events in the order they occurred.
func (q *Queue) Oldest(n int) []Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Event(nil), q.events[:min(n, len(q.events))]...)
}

// Remove drops the first n events once the server has stored them.
func (q *Queue) Remove(n int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	n = min(n, len(q.events))
	next := append([]Event(nil), q.events[n:]...)
	if err := writeAtomic(q.path, next); err != nil {
		return fmt.Errorf("remove uploaded offline audit events: %w", err)
	}
	q.events = next
	return nil
}

// HasOperation reports whether an event for operationID is queued.
func (q *Queue) HasOperation(operationID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, event := range q.events {
		if event.OperationID == operationID {
			return true
		}
	}
	return false
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}

func writeAtomic(path string, events []Event) error {
	data, err := json.Marshal(events)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
