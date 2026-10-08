// Package nodelifecycle maintains one authorized relay node's control lease.
package nodelifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
)

var ErrControl = errors.New("relay control authority unavailable or expired")
var ErrFenced = errors.New("relay node authority rejected")

const Interval = 3 * time.Second
const leaseLimit = 15 * time.Second

type Config struct {
	URL, Credential, NodeID, StatePath string
	ExpectedGeneration                 uint64
	HTTP                               *http.Client
	ControlTrace                       func(context.Context, string) (string, string, func(string, string))
}
type Lease struct {
	NodeID        string                      `json:"node_id"`
	Generation    uint64                      `json:"node_generation"`
	ProcessEpoch  string                      `json:"process_epoch"`
	ExpiresAt     int64                       `json:"expires_at"`
	CapacityLimit uint64                      `json:"capacity_limit"`
	Revocations   []derpquic.AuthoritySubject `json:"revocations"`
	PeerRelay     *derpquic.ServiceDescriptor `json:"peer_relay,omitempty"`
}
type diskState struct {
	NodeID       string `json:"node_id"`
	Generation   uint64 `json:"generation"`
	PendingEpoch string `json:"pending_epoch"`
}
type Client struct {
	cfg   Config
	lease Lease
	lock  *os.File
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(cfg.Credential) < 32 || cfg.NodeID == "" || cfg.StatePath == "" || cfg.ExpectedGeneration == 0 {
		return nil, ErrControl
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		copy := *cfg.HTTP
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		cfg.HTTP = &copy
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &Client{cfg: cfg}, nil
}

func (c *Client) post(ctx context.Context, path string, in any) (out Lease, resultErr error) {
	trace := ""
	reference := ""
	finish := func(string, string) {}
	if c.cfg.ControlTrace != nil {
		trace, reference, finish = c.cfg.ControlTrace(ctx, "dependency_health")
	}
	defer func() {
		outcome, code := "success", "ok"
		if resultErr != nil {
			outcome, code = "failed", "unavailable"
			if errors.Is(resultErr, context.Canceled) {
				outcome, code = "canceled", "shutdown"
			} else if errors.Is(resultErr, ErrFenced) {
				outcome, code = "rejected", "unauthorized"
			}
		}
		finish(outcome, code)
	}()
	body, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+path, bytes.NewReader(body))
	if err != nil {
		return out, ErrControl
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Credential)
	if trace != "" {
		req.Header.Set("sentry-trace", trace)
	}
	if reference != "" {
		req.Header.Set("Support-Reference", reference)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return out, ErrControl
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict {
		return out, ErrFenced
	}
	if resp.StatusCode != http.StatusOK {
		return out, ErrControl
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil || len(raw) >= 256<<10 {
		return out, ErrFenced
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || d.Decode(new(any)) != io.EOF || out.NodeID != c.cfg.NodeID || out.Generation == 0 || out.ProcessEpoch == "" || out.CapacityLimit == 0 || out.CapacityLimit > derpquic.MaxConnections || len(out.Revocations) > derpquic.MaxConnections {
		return Lease{}, ErrFenced
	}
	now := time.Now()
	if out.ExpiresAt <= now.Unix() || out.ExpiresAt > now.Add(leaseLimit).Unix() {
		return Lease{}, ErrFenced
	}
	if out.PeerRelay != nil && !out.PeerRelay.Valid() {
		return Lease{}, ErrFenced
	}
	return out, nil
}

func (c *Client) save(s diskState) error {
	dir := filepath.Dir(c.cfg.StatePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ErrControl
	}
	f, err := os.CreateTemp(dir, ".relay-state-")
	if err != nil {
		return ErrControl
	}
	name := f.Name()
	defer os.Remove(name)
	b, _ := json.Marshal(s)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrControl
	}
	if os.Rename(name, c.cfg.StatePath) != nil {
		return ErrControl
	}
	directory, err := os.Open(dir)
	if err != nil {
		return ErrControl
	}
	defer directory.Close()
	if directory.Sync() != nil {
		return ErrControl
	}
	return nil
}

func (c *Client) Start(ctx context.Context) (Lease, error) {
	if c.lock == nil {
		if os.MkdirAll(filepath.Dir(c.cfg.StatePath), 0700) != nil {
			return Lease{}, ErrControl
		}
		lock, err := lockState(c.cfg.StatePath + ".lock")
		if err != nil {
			return Lease{}, ErrControl
		}
		c.lock = lock
	}
	s := diskState{NodeID: c.cfg.NodeID, Generation: c.cfg.ExpectedGeneration}
	if info, err := os.Stat(c.cfg.StatePath); err == nil {
		if info.Size() > 4096 || info.Mode().Perm()&0077 != 0 {
			return Lease{}, ErrControl
		}
		b, err := os.ReadFile(c.cfg.StatePath)
		if err != nil || json.Unmarshal(b, &s) != nil || s.NodeID != c.cfg.NodeID || s.Generation == 0 {
			return Lease{}, ErrControl
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Lease{}, ErrControl
	}
	if s.PendingEpoch == "" {
		var epoch [16]byte
		rand.Read(epoch[:])
		s.PendingEpoch = hex.EncodeToString(epoch[:])
		if c.save(s) != nil {
			return Lease{}, ErrControl
		}
	}
	out, err := c.post(ctx, "/v1/relay/nodes/start", map[string]any{"node_id": s.NodeID, "expected_generation": s.Generation, "process_epoch": s.PendingEpoch})
	if err != nil || out.Generation != s.Generation+1 || out.ProcessEpoch != s.PendingEpoch {
		return Lease{}, ErrControl
	}
	s.Generation = out.Generation
	s.PendingEpoch = ""
	if c.save(s) != nil {
		return Lease{}, ErrControl
	}
	c.lease = out
	return out, nil
}

// Close releases the process ownership lock after Run has stopped.
func (c *Client) Close() error {
	if c.lock != nil {
		err := c.lock.Close()
		c.lock = nil
		return err
	}
	return nil
}

func (c *Client) Observe(ctx context.Context, server *derpquic.Server, draining bool) error {
	if !server.MatchesControlService(c.lease.PeerRelay) {
		return ErrFenced
	}
	stats := server.Snapshot()
	draining = draining || stats.Draining
	if c.lease.Generation == 0 || time.Now().Unix() >= c.lease.ExpiresAt {
		return ErrControl
	}
	subjects := server.AuthoritySubjects()
	out, err := c.post(ctx, "/v1/relay/nodes/observe", map[string]any{"node_id": c.lease.NodeID, "node_generation": c.lease.Generation, "process_epoch": c.lease.ProcessEpoch, "ready": server.Ready() && !draining, "draining": draining, "capacity_used": stats.Connections, "subjects": subjects})
	if err != nil {
		return err
	}
	if out.Generation != c.lease.Generation || out.ProcessEpoch != c.lease.ProcessEpoch || out.CapacityLimit != c.lease.CapacityLimit || !server.MatchesControlService(out.PeerRelay) {
		return ErrFenced
	}
	known := map[string]uint64{}
	for _, subject := range subjects {
		known[subject.AccountID+"\x00"+subject.EndpointID] = subject.Generation
	}
	seen := map[string]bool{}
	for _, rev := range out.Revocations {
		k := rev.AccountID + "\x00" + rev.EndpointID
		if known[k] == 0 || rev.Generation <= known[k] || seen[k] {
			return ErrFenced
		}
		seen[k] = true
	}
	for _, rev := range out.Revocations {
		if server.Revoke(rev.AccountID, rev.EndpointID, rev.Generation) != nil {
			return ErrControl
		}
	}
	c.lease = out
	return nil
}

// Run tolerates transient control loss only within the acknowledged lease.
func (c *Client) Run(ctx context.Context, server *derpquic.Server) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		deadline := time.Unix(c.lease.ExpiresAt, 0)
		if !time.Now().Before(deadline) {
			server.Drain(time.Now())
			server.Close()
			return ErrControl
		}
		observe, stop := context.WithDeadline(ctx, deadline)
		err := c.Observe(observe, server, false)
		stop()
		if errors.Is(err, ErrFenced) {
			server.Drain(time.Now())
			server.Close()
			return err
		}
		if err != nil && !time.Now().Before(deadline) {
			server.Drain(time.Now())
			server.Close()
			return ErrControl
		}
		delay := Interval
		if remain := time.Until(time.Unix(c.lease.ExpiresAt, 0)); remain < delay {
			delay = remain
		}
		if delay < 0 {
			delay = 0
		}
		timer.Reset(delay)
	}
}
