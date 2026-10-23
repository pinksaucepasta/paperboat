// Package machineevents lets a machine learn about server-side changes to what it caches without
// polling at a short interval. One Hub per process holds a single long request to the control plane
// (POST /v1/machine-events/wait). The server answers with the generation of every topic that moved
// past the generation the machine last saw, or with 204 after about 30 seconds. Generations carry no
// data: a woken subscriber refreshes through its own endpoint.
//
// While the wait loop is healthy, a subscriber's own poll becomes a slow safety net; while it is not,
// subscribers poll at their normal interval, so a failing wait loop never makes a machine slower than
// before it existed.
package machineevents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jitterrand "math/rand/v2"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

// Path is the control-plane endpoint a Hub waits on.
const Path = "/v1/machine-events/wait"

// Topic names one family of server-side changes.
type Topic string

const (
	TopicMachineAccess Topic = "machine_access"
	TopicPeerNetwork   Topic = "peer_network"
	TopicPreviews      Topic = "previews"
	TopicTunnels       Topic = "tunnels"
	TopicConfig        Topic = "config"
	TopicInventory     Topic = "inventory"
	// TopicMachineServices moves when a machine in this machine's audience publishes a changed
	// service snapshot (ports).
	TopicMachineServices Topic = "machine_services"
)

const (
	// DefaultFallbackInterval is how often a subscriber still polls while the wait loop is healthy.
	DefaultFallbackInterval = time.Minute
	// requestTimeout exceeds the 30 s the server holds a request, with room for the round trip.
	requestTimeout = 40 * time.Second
	// WaitTimeout bounds one wait made by a caller's own client; see requestTimeout.
	WaitTimeout   = requestTimeout
	minBackoff    = time.Second
	maxBackoff    = 30 * time.Second
	responseLimit = 4 << 10
)

// ErrInvalid reports a Hub configured without what it needs.
var ErrInvalid = errors.New("machine events configuration is invalid")

// Config wires a Hub to the control plane and the machine's identity.
type Config struct {
	ControlURL string
	Tokens     interface {
		Token(context.Context) (string, error)
	}
	Proofs interface {
		Proof(ctx context.Context, operationID, method, path string, body []byte) ([]byte, error)
	}
	OperationID func() (string, error)
	Transport   http.RoundTripper
	// FallbackInterval is the safety-net poll interval of subscribers while the loop is healthy.
	FallbackInterval time.Duration
}

// WaitFunc performs one wait request: it returns the topics whose generation is above the one given,
// or none after the server's hold time.
type WaitFunc func(ctx context.Context, known map[Topic]int64) (map[Topic]int64, error)

// Hub runs the wait loop and wakes subscribers.
type Hub struct {
	wait     WaitFunc
	fallback time.Duration

	mu      sync.Mutex
	subs    map[Topic][]*Subscription
	known   map[Topic]int64
	healthy bool
	changed chan struct{} // closed and replaced when a topic is first subscribed
	cancel  context.CancelFunc
	done    chan struct{}
}

// New builds a hub for a host that authenticates with its machine identity. The loop starts with Start.
func New(config Config) (*Hub, error) {
	parsed, err := url.Parse(config.ControlURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || config.Tokens == nil || config.Proofs == nil || config.OperationID == nil || config.Transport == nil {
		return nil, ErrInvalid
	}
	endpoint := strings.TrimRight(config.ControlURL, "/") + Path
	signed := &signedWaiter{
		endpoint: endpoint, tokens: config.Tokens, proofs: config.Proofs, operationID: config.OperationID,
		client: &http.Client{
			Transport: errorreport.TransportOperation(config.Transport, endpoint, "machine_events"), Timeout: requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("machine events redirect refused") },
		},
	}
	return NewWithWaiter(signed.wait, config.FallbackInterval)
}

// UserWaiter adapts a wait that speaks plain topic names (the CLI's API client) to a WaitFunc and
// bounds each request by WaitTimeout.
func UserWaiter(wait func(ctx context.Context, known map[string]int64) (map[string]int64, error)) WaitFunc {
	return func(ctx context.Context, known map[Topic]int64) (map[Topic]int64, error) {
		names := make(map[string]int64, len(known))
		for topic, generation := range known {
			names[string(topic)] = generation
		}
		ctx, cancel := context.WithTimeout(ctx, WaitTimeout)
		defer cancel()
		moved, err := wait(ctx, names)
		if err != nil {
			return nil, err
		}
		topics := make(map[Topic]int64, len(moved))
		for name, generation := range moved {
			topics[Topic(name)] = generation
		}
		return topics, nil
	}
}

// NewWithWaiter builds a hub around any wait request, for callers that authenticate another way
// (the signed-in CLI daemon waits with the user's credential). A zero fallback uses the default.
func NewWithWaiter(wait WaitFunc, fallback time.Duration) (*Hub, error) {
	if wait == nil || fallback < 0 {
		return nil, ErrInvalid
	}
	if fallback == 0 {
		fallback = DefaultFallbackInterval
	}
	return &Hub{wait: wait, fallback: fallback, subs: make(map[Topic][]*Subscription), known: make(map[Topic]int64), changed: make(chan struct{})}, nil
}

// Subscribe registers interest in a topic. Subscribe before Start, or at any time; a topic that is
// already known to have changed wakes the new subscriber once, so it refreshes right away.
func (h *Hub) Subscribe(topic Topic) *Subscription {
	if h == nil {
		return nil
	}
	sub := &Subscription{hub: h, wake: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, subscribed := h.known[topic]; !subscribed {
		h.known[topic] = 0
		close(h.changed)
		h.changed = make(chan struct{})
	}
	h.subs[topic] = append(h.subs[topic], sub)
	return sub
}

// Healthy reports whether the last wait request succeeded.
func (h *Hub) Healthy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy
}

// Start runs the wait loop until Shutdown or ctx ends.
func (h *Hub) Start(ctx context.Context) error {
	if h == nil || ctx == nil {
		return ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done != nil {
		return errors.New("machine events already started")
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.cancel, h.done = cancel, make(chan struct{})
	go func() { defer close(h.done); h.run(runCtx) }()
	return nil
}

// Shutdown stops the loop and waits for it.
func (h *Hub) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hub) run(ctx context.Context) {
	backoff := time.Duration(0)
	failing := false
	for ctx.Err() == nil {
		topics, changed := h.snapshot()
		if len(topics) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
			continue
		}
		moved, err := h.wait(ctx, topics)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			h.setHealthy(false)
			if !failing {
				failing = true
				errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "machine_events", "delivery", "control_request_failed", err)
			}
			backoff = min(max(backoff*2, minBackoff), maxBackoff)
			timer := time.NewTimer(backoff + time.Duration(jitterrand.Int64N(int64(backoff/2)+1)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		failing, backoff = false, 0
		h.setHealthy(true)
		h.deliver(moved)
	}
}

func (h *Hub) snapshot() (map[Topic]int64, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topics := make(map[Topic]int64, len(h.known))
	for topic, generation := range h.known {
		topics[topic] = generation
	}
	return topics, h.changed
}

func (h *Hub) setHealthy(value bool) {
	h.mu.Lock()
	h.healthy = value
	h.mu.Unlock()
}

// deliver records the new generations and wakes the topics' subscribers.
func (h *Hub) deliver(moved map[Topic]int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for topic, generation := range moved {
		if generation <= h.known[topic] {
			continue
		}
		h.known[topic] = generation
		for _, sub := range h.subs[topic] {
			sub.notify()
		}
	}
}

// signedWaiter is the machine-identity wait request.
type signedWaiter struct {
	endpoint string
	tokens   interface {
		Token(context.Context) (string, error)
	}
	proofs interface {
		Proof(ctx context.Context, operationID, method, path string, body []byte) ([]byte, error)
	}
	operationID func() (string, error)
	client      *http.Client
}

func (w *signedWaiter) wait(ctx context.Context, topics map[Topic]int64) (map[Topic]int64, error) {
	operationID, err := w.operationID()
	if err != nil || operationID == "" {
		return nil, fmt.Errorf("machine events operation id: %w", errors.Join(ErrInvalid, err))
	}
	body, err := json.Marshal(struct {
		Topics map[Topic]int64 `json:"topics"`
	}{topics})
	if err != nil {
		return nil, err
	}
	token, err := w.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("machine events identity: %w", err)
	}
	proof, err := w.proofs.Proof(ctx, operationID, http.MethodPost, Path, body)
	if err != nil {
		return nil, fmt.Errorf("machine events proof: %w", err)
	}
	request, err := http.NewRequestWithContext(httptransport.LongPoll(ctx), http.MethodPost, w.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Paperboat-Machine-Identity", token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Idempotency-Key", operationID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := w.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var answer struct {
			Topics map[Topic]int64 `json:"topics"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&answer); err != nil {
			return nil, fmt.Errorf("machine events response: %w", err)
		}
		return answer.Topics, nil
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
		return nil, errorreport.HTTPStatusFailure(response)
	}
}

// Subscription is one consumer's view of a topic. A nil Subscription is valid and means the consumer
// polls at its own interval, as before machine events existed.
type Subscription struct {
	hub  *Hub
	wake chan struct{}
}

func (s *Subscription) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Wait blocks until the consumer should refresh and reports false when ctx ended first. It returns
// at once when the topic changed, after base when the wait loop is unhealthy, and after the hub's
// fallback interval otherwise.
func (s *Subscription) Wait(ctx context.Context, base time.Duration) bool {
	if s == nil {
		timer := time.NewTimer(base)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	started := time.Now()
	timer := time.NewTimer(base)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.wake:
			return true
		case <-timer.C:
			if !s.hub.Healthy() || time.Since(started) >= s.hub.fallback {
				return true
			}
			timer.Reset(base)
		}
	}
}

// Healthy reports whether the hub's wait loop is currently receiving changes; a nil Subscription never is.
func (s *Subscription) Healthy() bool {
	return s != nil && s.hub.Healthy()
}

// Changed exposes wake-ups for consumers that already have their own loop and timer.
func (s *Subscription) Changed() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.wake
}
