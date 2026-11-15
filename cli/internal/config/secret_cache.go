package config

import (
	"sync"
	"time"
)

// secretCache remembers keychain reads for ttl: the macOS daemon ran `security find-generic-password` for the account
// credential on every API request, 16 process spawns a minute on an idle machine (PR-03). Another process's rewrite
// is seen after ttl (a rewrite gets a new reference anyway); this process's own Set and Delete take effect at once.
type secretCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]cachedSecret
}

type cachedSecret struct {
	value string
	at    time.Time
}

const maxCachedSecrets = 32

func (c *secretCache) get(ref string, read func() (string, error)) (string, error) {
	c.mu.Lock()
	entry, ok := c.entries[ref]
	fresh := ok && c.now().Sub(entry.at) < c.ttl
	c.mu.Unlock()
	if fresh {
		return entry.value, nil
	}
	value, err := read()
	if err == nil {
		c.set(ref, value)
	}
	return value, err
}

func (c *secretCache) set(ref, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= maxCachedSecrets {
		c.entries = map[string]cachedSecret{}
	}
	c.entries[ref] = cachedSecret{value: value, at: c.now()}
}

func (c *secretCache) drop(ref string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, ref)
}
