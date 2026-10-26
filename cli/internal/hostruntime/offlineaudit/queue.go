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
)

const (
	// MaxEvents and MaxAge bound the backlog (PLAN 3b "Offline-capable authority" point 5); the
	// server accepts events up to MaxAge old.
	MaxEvents = 10_000
	MaxAge    = 7 * 24 * time.Hour

	KindStarted = "offline_session_started"
	KindRefused = "offline_session_refused"

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

type Event struct {
	EventID        string    `json:"event_id"`
	Kind           string    `json:"kind"`
	Capability     string    `json:"capability"`
	PeerEndpointID string    `json:"peer_endpoint_id"`
	PeerAccountID  string    `json:"peer_account_id"`
	OperationID    string    `json:"operation_id"`
	OccurredAt     time.Time `json:"occurred_at"`
	Reason         string    `json:"reason,omitempty"`
	// The fields below belong to the ssh_session_* kinds: Origin says whether Paperboat was
	// reachable, SessionID (an end's) is the started event's EventID.
	Origin     string `json:"origin,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	BytesIn    int64  `json:"bytes_in,omitempty"`
	BytesOut   int64  `json:"bytes_out,omitempty"`
}

// Queue is the one owner of the backlog file.
type Queue struct {
	path    string
	now     func() time.Time
	mu      sync.Mutex
	events  []Event
	changed chan struct{}
}

func Open(path string, now func() time.Time) (*Queue, error) {
	if now == nil {
		now = time.Now
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
	temporary, err := os.CreateTemp(filepath.Dir(path), ".offline-audit-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	_, err = temporary.Write(data)
	err = errors.Join(err, temporary.Sync(), temporary.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
