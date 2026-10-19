// Package machineevents tells a host that something it caches has changed, without the host polling.
// A write that changes what a machine reads calls Publish inside its transaction: that bumps the
// persisted generation of the topic for each machine and wakes the requests waiting on them.
// A host waits with the generations it last saw and is answered the moment any is higher.
package machineevents

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Topics a host can wait on. A topic carries no data, only "this changed".
const (
	TopicMachineAccess = "machine_access"
	TopicPeerNetwork   = "peer_network"
	TopicPreviews      = "previews"
	TopicTunnels       = "tunnels"
	TopicConfig        = "config"
	TopicInventory     = "inventory"
)

var topics = []string{TopicMachineAccess, TopicPeerNetwork, TopicPreviews, TopicTunnels, TopicConfig, TopicInventory}

// ValidTopic reports whether name is a topic a host may wait on.
func ValidTopic(name string) bool { return slices.Contains(topics, name) }

const (
	// MaxWait is the longest a request is held before answering "no change".
	MaxWait = 30 * time.Second
	// recheckInterval re-reads the generations while waiting, so a missed notification (a listener
	// reconnecting) delays a change by seconds instead of losing it.
	recheckInterval = 5 * time.Second
	maxMachinesPer  = 1024
	channel         = "machine_events"
)

var ErrInvalid = errors.New("invalid machine event request")

// Execer is the part of a transaction or pool Publish needs.
type Execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// Publish records that topic changed for the given machines and wakes anyone waiting. Unknown machine
// ids are ignored. Call it in the transaction that makes the change: a rolled-back write publishes
// nothing, and the wake-up is delivered only once the generation is committed.
func Publish(ctx context.Context, tx Execer, machineIDs []string, topic string) error {
	if !ValidTopic(topic) {
		return ErrInvalid
	}
	if len(machineIDs) == 0 {
		return nil
	}
	if len(machineIDs) > maxMachinesPer {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `SELECT paperboat.publish_machine_topic($1::text[],$2)`, machineIDs, topic)
	return err
}

// Hub holds waiting requests and wakes them on notifications from PostgreSQL.
type Hub struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewHub(pool *pgxpool.Pool) *Hub {
	return &Hub{pool: pool, subs: map[string]map[chan struct{}]struct{}{}}
}

func (h *Hub) subscribe(machineID string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[machineID] == nil {
		h.subs[machineID] = map[chan struct{}]struct{}{}
	}
	h.subs[machineID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[machineID], ch)
		if len(h.subs[machineID]) == 0 {
			delete(h.subs, machineID)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) wake(machineID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[machineID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) wakeAll() {
	h.mu.Lock()
	machines := make([]string, 0, len(h.subs))
	for id := range h.subs {
		machines = append(machines, id)
	}
	h.mu.Unlock()
	for _, id := range machines {
		h.wake(id)
	}
}

// ListenFor listens for notifications for up to d and then returns, so a supervising worker sees a
// finished iteration every d. Waiters are woken at the start of each session and when a session
// fails, so nothing published while no listener was attached is missed. A failed session is not an
// error for the worker: it pauses briefly and the next session reconnects.
func (h *Hub) ListenFor(ctx context.Context, d time.Duration) error {
	session, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	if err := h.listen(session); err != nil && session.Err() == nil {
		h.wakeAll()
		select {
		case <-session.Done():
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	h.wakeAll() // anything published before LISTEN took effect
	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			// A broken connection must not go back to the pool.
			_ = conn.Conn().Close(context.Background())
			return err
		}
		h.wake(notification.Payload)
	}
}

// Wait returns the topics whose generation is higher than the one the host knows (a topic missing
// from known counts as 0), as soon as there is one. It returns an empty map when maxWait passes with
// no change, and ctx's error if the request is abandoned.
func (h *Hub) Wait(ctx context.Context, machineID string, known map[string]int64, maxWait time.Duration) (map[string]int64, error) {
	if machineID == "" || maxWait <= 0 || maxWait > MaxWait {
		return nil, ErrInvalid
	}
	for name, generation := range known {
		if !ValidTopic(name) || generation < 0 {
			return nil, ErrInvalid
		}
	}
	wake, unsubscribe := h.subscribe(machineID)
	defer unsubscribe()
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	recheck := time.NewTicker(recheckInterval)
	defer recheck.Stop()
	for {
		changed, err := h.changed(ctx, machineID, known)
		if err != nil || len(changed) > 0 {
			return changed, err
		}
		select {
		case <-wake:
		case <-recheck.C:
		case <-deadline.C:
			return map[string]int64{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (h *Hub) changed(ctx context.Context, machineID string, known map[string]int64) (map[string]int64, error) {
	rows, err := h.pool.Query(ctx, `SELECT topic,generation FROM machine_topic_generations WHERE machine_id=$1`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changed := map[string]int64{}
	for rows.Next() {
		var topic string
		var generation int64
		if err := rows.Scan(&topic, &generation); err != nil {
			return nil, err
		}
		if generation > known[topic] {
			changed[topic] = generation
		}
	}
	return changed, rows.Err()
}
