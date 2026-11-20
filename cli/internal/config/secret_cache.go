package config

import (
	"strings"
	"sync"
	"time"
)

// secretCache remembers keychain reads for ttl: the macOS daemon ran `security find-generic-password` for the account
// credential on every API request, 16 process spawns a minute on an idle machine (PR-03). Another process's rewrite
// is seen after ttl; this process's own Set and Delete take effect at once. Refresh tokens are never cached: a refresh
// rewrites the same reference, and a token another process had already rotated, sent with a new attempt id, made the
// server revoke the session as refresh_replay (test-m 2026-11-16, twice).
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

// secretReads is the process's keychain read cache (macOS only; nil elsewhere).
var secretReads *secretCache

func (c *secretCache) dropOlderThan(t time.Time, ref string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[ref]; ok && entry.at.Before(t) {
		delete(c.entries, ref)
	}
}

func uncachedSecret(ref string) bool {
	return strings.HasSuffix(ref, "-refresh") || strings.HasPrefix(ref, "refresh-attempt-")
}

func (c *secretCache) get(ref string, read func() (string, error)) (string, error) {
	if uncachedSecret(ref) {
		return read()
	}
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
	if uncachedSecret(ref) {
		return
	}
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
