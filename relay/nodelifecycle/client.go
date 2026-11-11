// Package nodelifecycle maintains one authorized relay node's control lease.
package nodelifecycle

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/jwks"
	"github.com/pinksaucepasta/paperboat/shared/wirejson"
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
	ControlFailure                     func(context.Context, string, error)
	// Trust remembers withdrawn issuer key ids for the process; main shares the one that read the startup
	// file. Nil starts empty.
	Trust *jwks.Trust
}
type Lease struct {
	NodeID        string                      `json:"node_id"`
	Generation    uint64                      `json:"node_generation"`
	ProcessEpoch  string                      `json:"process_epoch"`
	ExpiresAt     int64                       `json:"expires_at"`
	CapacityLimit uint64                      `json:"capacity_limit"`
	Revocations   []Revocation                `json:"revocations"`
	PeerRelay     *derpquic.ServiceDescriptor `json:"peer_relay,omitempty"`
	// QuotaDenied lists accounts the control plane reports out of bandwidth allowance; the relay
	// refuses them (derpquic.ErrQuota) until a later observation no longer lists them.
	QuotaDenied []string `json:"quota_denied,omitempty"`
	// SubjectsRequired asks for the full subjects list on the next observation: the control plane has
	// no stored set for this process or its digest differs. Such an answer carries no quota list.
	SubjectsRequired bool `json:"subjects_required,omitempty"`
	// JWKS is the issuer trust document (keys plus withdrawn_key_ids); the relay trusts exactly its keys,
	// so a rotation or withdrawal applies within one observation.
	JWKS json.RawMessage `json:"jwks,omitempty"`
}

// Revocation fences one endpoint this relay holds: grants below MinGeneration, the first
// generation still valid, are closed and refused. The control plane resends it every observation
// until the endpoint re-keys; an applied one is a no-op.
type Revocation struct {
	AccountID     string `json:"account_id"`
	EndpointID    string `json:"endpoint_id"`
	MinGeneration uint64 `json:"min_generation"`
}
type diskState struct {
	NodeID       string `json:"node_id"`
	Generation   uint64 `json:"generation"`
	PendingEpoch string `json:"pending_epoch"`
}
type Client struct {
	cfg Config
	// mu serializes observations: Run polls while a grant naming an unknown key id may refresh out of band.
	mu    sync.Mutex
	lease Lease
	// jwks is the trust document last applied, so a steady one is not parsed again; startKeys are the keys
	// the start answer's document left (nil when it carried none).
	jwks      []byte
	startKeys map[string]ed25519.PublicKey
	lock      *os.File
	// sentSubjects is the digest of the subjects list the control plane last accepted in full; an
	// observation sends only the digest while the set is unchanged (~400 KB every 3 s at 4,096
	// connections otherwise).
	sentSubjects string
}

// subjectsDigest is the hex SHA-256 of the sorted "account\x00endpoint\n" membership lines (the
// control plane computes the same). Membership, not grant generations, so a steady set keeps it.
func subjectsDigest(subjects []derpquic.AuthoritySubject) string {
	lines := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		lines = append(lines, subject.AccountID+"\x00"+subject.EndpointID+"\n")
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])
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
	if cfg.Trust == nil {
		cfg.Trust = &jwks.Trust{}
	}
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
		if resultErr != nil && c.cfg.ControlFailure != nil {
			c.cfg.ControlFailure(ctx, reference, resultErr)
			code = "control_request_failed"
		}
		finish(outcome, code)
	}()
	body, err := json.Marshal(in)
	if err != nil {
		return out, failure(err, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+path, bytes.NewReader(body))
	if err != nil {
		return out, failure(err, 0, false)
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
		return out, failure(err, 0, false)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict {
		return out, failure(nil, resp.StatusCode, true)
	}
	if resp.StatusCode != http.StatusOK {
		return out, failure(nil, resp.StatusCode, false)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return out, failure(err, 0, true)
	}
	if len(raw) >= 256<<10 {
		return out, ErrFenced
	}
	// A lease is control data over the authenticated channel, not a signed claim: a field the server
	// adds later must not fence every relay (AGENTS.md, internal/wirejson).
	if err := wirejson.Decode(raw, &out); err != nil {
		return Lease{}, failure(err, 0, true)
	}
	if out.NodeID != c.cfg.NodeID || out.Generation == 0 || out.ProcessEpoch == "" || out.CapacityLimit == 0 || out.CapacityLimit > derpquic.MaxConnections || len(out.Revocations) > derpquic.MaxConnections {
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
		return failure(err, 0, false)
	}
	f, err := os.CreateTemp(dir, ".relay-state-")
	if err != nil {
		return failure(err, 0, false)
	}
	name := f.Name()
	defer os.Remove(name)
	b, _ := json.Marshal(s)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return failure(err, 0, false)
	}
	if closeErr != nil {
		return failure(closeErr, 0, false)
	}
	if err := os.Rename(name, c.cfg.StatePath); err != nil {
		return failure(err, 0, false)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return failure(err, 0, false)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return failure(err, 0, false)
	}
	return nil
}

func (c *Client) Start(ctx context.Context) (Lease, error) {
	if c.lock == nil {
		if err := os.MkdirAll(filepath.Dir(c.cfg.StatePath), 0700); err != nil {
			return Lease{}, failure(err, 0, false)
		}
		lock, err := lockState(c.cfg.StatePath + ".lock")
		if err != nil {
			return Lease{}, failure(err, 0, false)
		}
		c.lock = lock
	}
	s := diskState{NodeID: c.cfg.NodeID, Generation: c.cfg.ExpectedGeneration}
	if info, err := os.Stat(c.cfg.StatePath); err == nil {
		if info.Size() > 4096 || info.Mode().Perm()&0077 != 0 {
			return Lease{}, ErrControl
		}
		b, err := os.ReadFile(c.cfg.StatePath)
		if err != nil {
			return Lease{}, failure(err, 0, false)
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return Lease{}, failure(err, 0, false)
		}
		if s.NodeID != c.cfg.NodeID || s.Generation == 0 {
			return Lease{}, ErrControl
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Lease{}, failure(err, 0, false)
	}
	if s.PendingEpoch == "" {
		var epoch [16]byte
		rand.Read(epoch[:])
		s.PendingEpoch = hex.EncodeToString(epoch[:])
		if err := c.save(s); err != nil {
			return Lease{}, err
		}
	}
	out, err := c.post(ctx, "/v1/relay/nodes/start", map[string]any{"node_id": s.NodeID, "expected_generation": s.Generation, "process_epoch": s.PendingEpoch})
	if err != nil {
		return Lease{}, err
	}
	if out.Generation != s.Generation+1 || out.ProcessEpoch != s.PendingEpoch {
		return Lease{}, ErrControl
	}
	// The control plane's current trust document outranks the startup file the process was given.
	if len(out.JWKS) != 0 {
		keys, err := c.cfg.Trust.Apply(out.JWKS)
		if err != nil {
			return Lease{}, ErrControl
		}
		c.startKeys, c.jwks = keys, out.JWKS
	}
	s.Generation = out.Generation
	s.PendingEpoch = ""
	if err := c.save(s); err != nil {
		return Lease{}, err
	}
	c.lease = out
	return out, nil
}

// StartKeys are the grant keys the start answer's trust document left, or nil when it carried none: the
// relay serves with them instead of a possibly stale startup file.
func (c *Client) StartKeys() map[string]ed25519.PublicKey { return c.startKeys }

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
	c.mu.Lock()
	defer c.mu.Unlock()
	if !server.MatchesControlService(c.lease.PeerRelay) {
		return ErrFenced
	}
	stats := server.Snapshot()
	draining = draining || stats.Draining
	if c.lease.Generation == 0 || time.Now().Unix() >= c.lease.ExpiresAt {
		return ErrControl
	}
	subjects := server.AuthoritySubjects()
	digest := subjectsDigest(subjects)
	body := map[string]any{"node_id": c.lease.NodeID, "node_generation": c.lease.Generation, "process_epoch": c.lease.ProcessEpoch, "ready": server.Ready() && !draining, "draining": draining, "capacity_used": stats.Connections, "subjects_digest": digest, "quota_denied_supported": true}
	full := digest != c.sentSubjects
	if full {
		body["subjects"] = append([]derpquic.AuthoritySubject{}, subjects...)
	}
	out, err := c.post(ctx, "/v1/relay/nodes/observe", body)
	if err != nil {
		return err
	}
	if out.Generation != c.lease.Generation || out.ProcessEpoch != c.lease.ProcessEpoch || out.CapacityLimit != c.lease.CapacityLimit || !server.MatchesControlService(out.PeerRelay) {
		return ErrFenced
	}
	// A revocation only restricts: one for an endpoint this relay does not hold fences its future
	// grants, a repeated one is a no-op (Server.Revoke keeps the highest), so neither drains the relay.
	// A malformed entry is a broken authority, not a transient failure: it fences before any is applied.
	for _, rev := range out.Revocations {
		if !derpquic.ValidRevocation(rev.AccountID, rev.EndpointID, rev.MinGeneration) {
			return ErrFenced
		}
	}
	for _, rev := range out.Revocations {
		if server.Revoke(rev.AccountID, rev.EndpointID, rev.MinGeneration) != nil {
			return ErrControl
		}
	}
	if len(out.JWKS) != 0 && !bytes.Equal(out.JWKS, c.jwks) {
		keys, err := c.cfg.Trust.Apply(out.JWKS)
		if err != nil || server.SetKeys(keys) != nil {
			return ErrControl
		}
		c.jwks = out.JWKS
	}
	if out.SubjectsRequired {
		// The control plane lost or never had this set: send it in full next time, and keep the
		// quota list, which such an answer does not carry.
		c.sentSubjects = ""
		out.QuotaDenied, out.SubjectsRequired = c.lease.QuotaDenied, false
	} else {
		if full {
			c.sentSubjects = digest
		}
		if server.SetQuotaDenied(out.QuotaDenied) != nil {
			return ErrControl
		}
	}
	c.lease = out
	return nil
}

func (c *Client) leaseExpiry() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lease.ExpiresAt
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
		deadline := time.Unix(c.leaseExpiry(), 0)
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
			return err
		}
		delay := Interval
		if remain := time.Until(time.Unix(c.leaseExpiry(), 0)); remain < delay {
			delay = remain
		}
		if delay < 0 {
			delay = 0
		}
		timer.Reset(delay)
	}
}
