package peersessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// usePolicyCache runs the service's policy LISTEN session for the test and makes each configuration read its own
// writes: before a cached document is read it sends a marker notification and waits until the listener has applied
// it. Notifications reach a listener in commit order, so every notification committed before the marker has been
// applied too. A test that changes state and asks again therefore gets a fresh document exactly when a notification
// named a party of the old one: a mutation path without one fails the test's own assertion on the new document.
// The relay node rows (relay_nodes.go) are reused for at most relayNodeTTL by design; the hook drops them too.
func usePolicyCache(t *testing.T, s *NetworkService, store *db.DB) {
	t.Helper()
	var mu sync.Mutex
	waiting := map[string]chan struct{}{}
	s.cache.mu.Lock()
	s.cache.observe = func(key string) {
		mu.Lock()
		if ch := waiting[key]; ch != nil {
			close(ch)
			delete(waiting, key)
		}
		mu.Unlock()
	}
	s.cache.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if s.ListenPolicy(ctx, store.Direct(), time.Second) != nil && ctx.Err() == nil {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	s.policySync = func(ctx context.Context) error {
		s.nodes.mu.Lock()
		s.nodes.entries = nil // relay rows are reused for relayNodeTTL; a test reads its own registry writes
		s.nodes.mu.Unlock()
		if _, listening := s.cache.begin(); !listening {
			return nil // nothing is cached or stored until the session is attached
		}
		raw := make([]byte, 8)
		_, _ = rand.Read(raw)
		marker := "policy-sync:" + hex.EncodeToString(raw)
		ch := make(chan struct{})
		mu.Lock()
		waiting[marker] = ch
		mu.Unlock()
		if _, err := store.SQL().ExecContext(ctx, `SELECT pg_notify('machine_events',$1)`, marker); err != nil {
			return err
		}
		select {
		case <-ch:
			return nil
		case <-time.After(10 * time.Second):
			t.Errorf("policy listener did not see %s", marker)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func cacheDoc(self string, peers ...string) *policyDoc {
	d := &policyDoc{self: networkBinding{Binding: peernetwork.Binding{AccountID: "acct_" + self, EndpointID: self}}, validUntil: time.Unix(2000, 0)}
	for _, p := range peers {
		d.peers = append(d.peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "acct_" + p, EndpointID: p, MachineID: "machine_" + p}}})
	}
	d.keys = policyKeys(d.self, d.peers)
	return d
}

func TestPolicyCacheServesUntilANotificationNamesAParty(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newPolicyCache()
	start, ok := c.begin()
	if ok {
		t.Fatal("cache stores before a LISTEN session is attached")
	}
	c.put("e1", cacheDoc("e1", "p1"), start)
	if c.get("e1", "", now) != nil {
		t.Fatal("stored without a listener")
	}
	c.reset(true)
	for _, key := range []string{"e1", "account:acct_e1", "p1", "machine_p1", "account:acct_p1"} {
		start, _ = c.begin()
		c.put("e1", cacheDoc("e1", "p1"), start)
		if c.get("e1", "", now) == nil {
			t.Fatalf("%s: not served from memory", key)
		}
		c.notify("account:unrelated")
		if c.get("e1", "", now) == nil {
			t.Fatalf("%s: an unrelated notification dropped the document", key)
		}
		c.notify(key)
		if c.get("e1", "", now) != nil {
			t.Fatalf("notification %s left the document in place", key)
		}
	}
	start, _ = c.begin()
	c.put("e1", cacheDoc("e1"), start)
	if c.get("e1", "other-live-set", now) != nil {
		t.Fatal("served for another admitted scope set")
	}
	if c.get("e1", "", time.Unix(2000, 0)) != nil {
		t.Fatal("served at its valid-until instant")
	}
	c.reset(false)
	if c.get("e1", "", now) != nil {
		t.Fatal("served after the listener lost its session")
	}
}

func TestPolicyCacheRefusesADocumentBuiltAcrossItsNotification(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newPolicyCache()
	c.reset(true)
	start, _ := c.begin()
	c.notify("p1") // the change committed while the build was reading
	c.put("e1", cacheDoc("e1", "p1"), start)
	if c.get("e1", "", now) != nil {
		t.Fatal("stored a document built across a notification for its peer")
	}
	start, _ = c.begin()
	c.notify("account:unrelated")
	c.put("e1", cacheDoc("e1", "p1"), start)
	if c.get("e1", "", now) == nil {
		t.Fatal("an unrelated notification during the build refused the document")
	}
	start, _ = c.begin()
	c.reset(true)
	c.put("e2", cacheDoc("e2"), start)
	if c.get("e2", "", now) != nil {
		t.Fatal("stored a document built across a listener reset")
	}
	start, _ = c.begin()
	for range recentNotices {
		c.notify("account:unrelated")
	}
	c.put("e3", cacheDoc("e3"), start)
	if c.get("e3", "", now) != nil {
		t.Fatal("stored a document built across more notifications than the cache remembers")
	}
}

func TestLiveKeyIsCanonical(t *testing.T) {
	a := liveKey(map[string][]string{"machine_access": {"b", "a", "a"}, "inspector": {"x"}})
	b := liveKey(map[string][]string{"inspector": {"x"}, "machine_access": {"a", "b"}})
	if a != b || a == liveKey(map[string][]string{"machine_access": {"a", "b"}}) {
		t.Fatalf("live keys %q %q", a, b)
	}
}
