package peersessions

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The peer policy cache (PLAN 3b / WHITEBOARD "in-memory peer policy, computed on change"). Each endpoint's policy
// part of its configuration (its own binding, every peer with its scopes, the refresh hint and the peers' owning
// teams) is built once from PostgreSQL and then served from memory until something it depends on changes.
//
// One invalidation owner: the machine_events notifications. The topic triggers (paperboat.publish_machine_topic /
// publish_account_topic, migrations 110, 150 and later) NOTIFY machine_events with a machine id or "account:<id>";
// migration 334 adds paperboat.publish_peer_policy, which sends "policy:<key>" from a trigger on every table the
// document reads (TestPeerPolicyInvalidationCoversEveryMutationPathOnPostgres checks each write). A cached document
// depends on its own endpoint and account and on every peer's endpoint, machine and account; a notification naming
// any of them drops it. Every server replica runs its own LISTEN session, so replicas stay in sync without talking
// to each other. A document also ends at the earliest time-based input it was built from (a grant or certificate
// expiry, a team's paid period) and after maxPolicyAge at most. Configuration requests still allocate their
// generation (one row update) and sign per request; relay rows are reused for relayNodeTTL (relay_nodes.go).
//
// Prior art: headscale compiles the policy once per change and keeps per-node filter rules in memory
// (hscontrol/policy/v2/policy.go, filterRulesMap/updateLocked); NetBird builds the account's network map in memory
// and pushes it (management/server/types/account_components.go). Both invalidate on the write path; here the write
// path is the database triggers, so writes from any replica, worker or migration are covered alike.

// maxPolicyAge bounds a cached document whose inputs did not change: defence in depth behind the notifications, not
// the mechanism. An idle daemon renews about once a minute, so this is roughly ten builds saved per endpoint.
const maxPolicyAge = 10 * time.Minute

// recentNotices is how many notifications the cache remembers to refuse a document built across one of them.
const recentNotices = 4096

// maxCachedPolicies bounds memory: a document is a few kB, so 200k endpoints is well under a GB.
const maxCachedPolicies = 200_000

type policyDoc struct {
	liveKey    string
	self       networkBinding
	peers      []networkPeer
	expiry     time.Time         // the configuration's refresh hint (exp), as built
	teamOf     map[string]string // machine id -> owning team ("" personal), for the relay quota decision
	validUntil time.Time
	keys       []string // sorted (policyKeys): put binary-searches it
}

type policyNotice struct {
	seq uint64
	key string
}

type policyCache struct {
	mu        sync.Mutex
	listening bool
	seq       uint64
	recent    []policyNotice // ring of the last recentNotices notifications, notification seq at seq%recentNotices
	docs      map[string]*policyDoc
	byKey     map[string]map[string]struct{}
	// hits and builds count served and built documents (tests and the capacity report read them).
	hits, builds uint64
	// observe, when set (tests), sees every notification key after it was applied.
	observe func(key string)
}

func newPolicyCache() *policyCache {
	return &policyCache{recent: make([]policyNotice, recentNotices), docs: map[string]*policyDoc{}, byKey: map[string]map[string]struct{}{}}
}

// begin returns the notification sequence a build starts at, and whether a document built now may be stored
// (only while a LISTEN session is attached: otherwise a change could go unseen).
func (c *policyCache) begin() (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq, c.listening
}

func (c *policyCache) get(endpoint, liveKey string, now time.Time) *policyDoc {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.docs[endpoint]
	if d == nil || !c.listening || d.liveKey != liveKey || !now.Before(d.validUntil) {
		c.builds++
		return nil
	}
	c.hits++
	return d
}

// put stores a document built from a snapshot taken after start, unless a notification for one of its keys
// arrived since then (the build may have read the state before that change), or the listener lost its session.
func (c *policyCache) put(endpoint string, d *policyDoc, start uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.listening || c.seq-start >= recentNotices {
		return
	}
	for seq := start + 1; seq <= c.seq; seq++ {
		if n := c.recent[seq%recentNotices]; n.seq == seq {
			if _, found := slices.BinarySearch(d.keys, n.key); found {
				return
			}
		}
	}
	c.dropLocked(endpoint)
	if len(c.docs) >= maxCachedPolicies {
		for id := range c.docs { // map order: an arbitrary victim
			c.dropLocked(id)
			break
		}
	}
	c.docs[endpoint] = d
	for _, k := range d.keys {
		if c.byKey[k] == nil {
			c.byKey[k] = map[string]struct{}{}
		}
		c.byKey[k][endpoint] = struct{}{}
	}
}

func (c *policyCache) dropLocked(endpoint string) {
	d := c.docs[endpoint]
	if d == nil {
		return
	}
	delete(c.docs, endpoint)
	for _, k := range d.keys {
		delete(c.byKey[k], endpoint)
		if len(c.byKey[k]) == 0 {
			delete(c.byKey, k)
		}
	}
}

// notify drops every document that depends on key (a machine id or "account:<id>").
func (c *policyCache) notify(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.recent[c.seq%recentNotices] = policyNotice{seq: c.seq, key: key}
	for endpoint := range c.byKey[key] {
		c.dropLocked(endpoint)
	}
	if c.observe != nil {
		c.observe(key)
	}
}

// reset drops everything: the listener (re)attached or lost its session, so notifications may have been missed.
func (c *policyCache) reset(listening bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listening = listening
	c.seq += recentNotices + 1 // builds in flight across the reset are not stored
	c.docs = map[string]*policyDoc{}
	c.byKey = map[string]map[string]struct{}{}
}

// policyKeys are the notification keys a document depends on: its own endpoint and account and every peer's
// endpoint, machine and account. A team change notifies the accounts of the members it affects (the audience
// functions of migration 150), which include the endpoint's own account.
func policyKeys(self networkBinding, peers []networkPeer) []string {
	keys := []string{self.EndpointID, "account:" + self.AccountID}
	if self.MachineID != "" {
		keys = append(keys, self.MachineID)
	}
	for _, p := range peers {
		keys = append(keys, p.Identity.EndpointID, "account:"+p.Identity.AccountID)
		if p.Identity.MachineID != "" {
			keys = append(keys, p.Identity.MachineID)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// liveKey is the canonical form of the admitted scopes a request reports: they decide which expired grants a
// document keeps, so a document is reused only for the same set.
func liveKey(live map[string][]string) string {
	kinds := make([]string, 0, len(live))
	for kind := range live {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	var b strings.Builder
	for _, kind := range kinds {
		ids := slices.Clone(live[kind])
		slices.Sort(ids)
		b.WriteString(kind)
		for _, id := range slices.Compact(ids) {
			b.WriteByte(0)
			b.WriteString(id)
		}
		b.WriteByte(1)
	}
	return b.String()
}

// ListenPolicy keeps the policy cache's LISTEN session on machine_events for up to d, then returns so the
// supervising worker sees an iteration. The connection is kept across sessions (a read timeout leaves a pgx
// connection usable and notifications queue on it meanwhile), so the cache lives across session boundaries. Any
// failure closes the connection and drops the whole cache; the next session starts empty.
func (s *NetworkService) ListenPolicy(ctx context.Context, direct *pgxpool.Pool, d time.Duration) error {
	s.policyListen.Lock()
	defer s.policyListen.Unlock()
	if s.policyConn == nil {
		pooled, err := direct.Acquire(ctx)
		if err != nil {
			return err
		}
		// Taken out of the pool: the session outlives any one request and must not hold up the pool's Close.
		conn := pooled.Hijack()
		if _, err = conn.Exec(ctx, "LISTEN machine_events"); err != nil {
			_ = conn.Close(context.Background())
			return err
		}
		s.policyConn = conn
		s.cache.reset(true) // anything published before LISTEN took effect is not in any document now
	}
	session, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for {
		n, err := s.policyConn.WaitForNotification(session)
		if err == nil {
			// "policy:<key>" comes from the peer policy triggers (migration 334), a bare key from the topic triggers.
			s.cache.notify(strings.TrimPrefix(n.Payload, "policy:"))
			continue
		}
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && !s.policyConn.IsClosed() {
			return nil
		}
		s.cache.reset(false)
		_ = s.policyConn.Close(context.Background())
		s.policyConn = nil
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}
