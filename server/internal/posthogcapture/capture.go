// Package posthogcapture sends the server's product events to PostHog: signed in, machine enrolled, team created
// and the first-use kinds the console's retention and adoption views read. It never blocks a request: Capture
// only queues, a bounded queue drops the oldest-unsent overflow, and a failed delivery is dropped, not retried.
// An event carries the account's server id and a few coarse labels, never an email, name, command or file name.
package posthogcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	queueSize      = 1000
	batchSize      = 50
	flushEvery     = 5 * time.Second
	requestTimeout = 5 * time.Second
	dailyLimit     = 100_000
	maxValue       = 64
)

// allowed are the only property names an event may carry; anything else is dropped before it is queued, so a
// caller can never leak a name, address or command by accident.
var allowed = map[string]bool{"feature": true, "platform": true, "role": true, "via": true, "plan": true, "state": true}

type Config struct {
	Host   string // https origin of PostHog's capture endpoint
	Key    string // the project's public capture key
	Client *http.Client
	Now    func() time.Time
}

// Event is one product event. UUID lets PostHog merge a redelivered event with its first delivery (eventually: the
// copies can both be visible for a while).
type Event struct {
	Name     string
	Distinct string // the account's server id
	At       time.Time
	UUID     string
	Props    map[string]string
}

type Client struct {
	cfg   Config
	queue chan Event
	now   func() time.Time
	mu    sync.Mutex
	day   string
	seen  map[string]bool
}

// New returns nil without a host and key; every method is a no-op on nil.
func New(cfg Config) *Client {
	if cfg.Host == "" || cfg.Key == "" {
		return nil
	}
	c := &Client{cfg: cfg, queue: make(chan Event, queueSize), now: cfg.Now, seen: map[string]bool{}}
	if c.now == nil {
		c.now = time.Now
	}
	if c.cfg.Client == nil {
		c.cfg.Client = &http.Client{}
	}
	hc := *c.cfg.Client
	hc.Timeout = requestTimeout
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.cfg.Client = &hc
	return c
}

// Capture queues the event and reports whether it was accepted; a full queue drops it.
func (c *Client) Capture(e Event) bool {
	if c == nil || e.Name == "" || e.Distinct == "" {
		return false
	}
	props := make(map[string]string, len(e.Props))
	for k, v := range e.Props {
		if allowed[k] {
			props[k] = v[:min(len(v), maxValue)]
		}
	}
	e.Props = props
	select {
	case c.queue <- e:
		return true
	default:
		return false
	}
}

// CaptureDaily is Capture at most once per account, event and property set per UTC day: the hot paths that see
// the same use every minute call this. The set is cleared each day and bounded; past the bound events are dropped.
func (c *Client) CaptureDaily(e Event) bool {
	if c == nil {
		return false
	}
	day := c.now().UTC().Format("2006-01-02")
	key := e.Distinct + "|" + e.Name + "|" + e.Props["feature"]
	c.mu.Lock()
	if c.day != day {
		c.day, c.seen = day, map[string]bool{}
	}
	dup := c.seen[key] || len(c.seen) >= dailyLimit
	if !dup {
		c.seen[key] = true
	}
	c.mu.Unlock()
	return !dup && c.Capture(e)
}

// Run delivers queued events in batches until ctx ends, then sends what is left once.
func (c *Client) Run(ctx context.Context) error {
	if c == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	var batch []Event
	send := func(ctx context.Context) {
		if len(batch) > 0 {
			c.deliver(ctx, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case e := <-c.queue:
			if batch = append(batch, e); len(batch) >= batchSize {
				send(ctx)
			}
		case <-tick.C:
			send(ctx)
		case <-ctx.Done():
			for len(c.queue) > 0 {
				batch = append(batch, <-c.queue)
			}
			// Whatever was queued or half-batched when the context ended goes out in batches of at most
			// batchSize, as it would have in the loop above.
			for len(batch) > 0 {
				flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
				n := min(len(batch), batchSize)
				c.deliver(flush, batch[:n])
				cancel()
				batch = batch[n:]
			}
			return ctx.Err()
		}
	}
}

type wireEvent struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Timestamp  string         `json:"timestamp"`
	UUID       string         `json:"uuid,omitempty"`
	Properties map[string]any `json:"properties"`
}

func (c *Client) body(batch []Event) ([]byte, error) {
	out := make([]wireEvent, 0, len(batch))
	for _, e := range batch {
		at := e.At
		if at.IsZero() {
			at = c.now()
		}
		// The server's own address must never become an account's country: GeoIP stays off for server events.
		props := map[string]any{"app": "server", "$geoip_disable": true, "$lib": "paperboat-server"}
		for k, v := range e.Props {
			props[k] = v
		}
		out = append(out, wireEvent{e.Name, e.Distinct, at.UTC().Format(time.RFC3339Nano), e.UUID, props})
	}
	return json.Marshal(map[string]any{"api_key": c.cfg.Key, "batch": out})
}

// deliver makes one attempt; a failure drops the batch (analytics never backs up the server).
func (c *Client) deliver(ctx context.Context, batch []Event) {
	body, err := c.body(batch)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.Host, "/")+"/batch/", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.cfg.Client.Do(req); err == nil {
		resp.Body.Close()
	}
}
