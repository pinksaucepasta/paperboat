package clientauthority

import (
	"context"
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

type cacheKey struct {
	account, client, machine string
	generation               uint64
}

type cacheEntry struct {
	authority   Authority
	refreshedAt time.Time
}

const trustRefreshInterval = 15 * time.Second

// localServeStaleFor is how long ResolveLocal answers from an older cached local identity while
// one background refresh replaces it, so a command after a short idle gap does not wait for two
// control-plane round trips before it can dial. Past it, or once the local certificate is about
// to expire, resolution waits for the server as before.
const localServeStaleFor = 2 * time.Minute

// localRefreshTimeout bounds one background local refresh.
const localRefreshTimeout = 30 * time.Second

// Cache retains endpoint authority metadata for the daemon lifetime. It never
// owns a carrier; machine-specific transport state is created only by an
// active consumer.
type Cache struct {
	mu         sync.Mutex
	entries    map[cacheKey]cacheEntry
	refreshing map[cacheKey]bool
	// resolveLocal is ResolveLocal; tests replace it to count and pace control-plane work.
	resolveLocal func(context.Context, Request) (Authority, error)
}

func NewCache() *Cache {
	return &Cache{entries: make(map[cacheKey]cacheEntry), refreshing: make(map[cacheKey]bool), resolveLocal: ResolveLocal}
}

func (c *Cache) Resolve(ctx context.Context, request Request) (Authority, error) {
	if c == nil {
		return Resolve(ctx, request)
	}
	key := cacheKey{account: request.AccountID, client: request.CLIClientSessionID, machine: request.MachineID, generation: request.MachineGeneration}
	now := request.Now.UTC()
	c.mu.Lock()
	if cached, ok := c.entries[key]; ok && !now.Before(cached.refreshedAt) &&
		now.Sub(cached.refreshedAt) < trustRefreshInterval &&
		cached.authority.MachineCertificate.Claims.ExpiresAt.After(now.Add(time.Second)) &&
		cached.authority.LocalCertificate.Claims.ExpiresAt.After(now.Add(time.Second)) {
		clone := cloneAuthority(cached.authority)
		c.mu.Unlock()
		return clone, nil
	}
	c.mu.Unlock()
	resolved, err := Resolve(ctx, request)
	if err != nil {
		return Authority{}, err
	}
	c.mu.Lock()
	if previous, ok := c.entries[key]; ok {
		previous.authority.Clear()
	}
	c.entries[key] = cacheEntry{authority: cloneAuthority(resolved), refreshedAt: now}
	c.mu.Unlock()
	return resolved, nil
}

// ResolveLocal caches this endpoint's own verified identity. Remote peers, including a
// teammate's shared machine, are authenticated by the native network bindings, so no machine
// is named here and nothing account-private to its owner is read.
func (c *Cache) ResolveLocal(ctx context.Context, request Request) (Authority, error) {
	if c == nil {
		return ResolveLocal(ctx, request)
	}
	key := cacheKey{account: request.AccountID, client: request.CLIClientSessionID}
	now := request.Now.UTC()
	c.mu.Lock()
	if cached, ok := c.entries[key]; ok && !now.Before(cached.refreshedAt) &&
		cached.authority.LocalCertificate.Claims.ExpiresAt.After(now.Add(time.Second)) {
		age := now.Sub(cached.refreshedAt)
		if age < trustRefreshInterval || age < localServeStaleFor && cached.authority.LocalCertificate.Claims.ExpiresAt.After(now.Add(localServeStaleFor)) {
			if age >= trustRefreshInterval && !c.refreshing[key] {
				c.refreshing[key] = true
				go c.refreshLocal(key, request)
			}
			clone := cloneAuthority(cached.authority)
			c.mu.Unlock()
			return clone, nil
		}
	}
	c.mu.Unlock()
	resolved, err := c.resolveLocal(ctx, request)
	if err != nil {
		return Authority{}, err
	}
	c.storeLocal(key, resolved, now)
	return resolved, nil
}

// refreshLocal replaces a stale local identity in the background. A failure leaves the stale
// entry to age out; the next resolution after localServeStaleFor then waits for the server.
func (c *Cache) refreshLocal(key cacheKey, request Request) {
	defer func() {
		c.mu.Lock()
		delete(c.refreshing, key)
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), localRefreshTimeout)
	defer cancel()
	request.Now = time.Now().UTC()
	resolved, err := c.resolveLocal(ctx, request)
	if err != nil {
		return
	}
	c.storeLocal(key, resolved, request.Now)
	resolved.Clear()
}

func (c *Cache) storeLocal(key cacheKey, resolved Authority, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous, ok := c.entries[key]; ok {
		if at.Before(previous.refreshedAt) {
			return
		}
		previous.authority.Clear()
	}
	c.entries[key] = cacheEntry{authority: cloneAuthority(resolved), refreshedAt: at}
}

func (c *Cache) InvalidateMachine(machineID string) {
	if c == nil || machineID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, authority := range c.entries {
		if key.machine == machineID {
			authority.authority.Clear()
			delete(c.entries, key)
		}
	}
}

func (c *Cache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, authority := range c.entries {
		authority.authority.Clear()
		delete(c.entries, key)
	}
}

func cloneAuthority(value Authority) Authority {
	return Authority{
		RootPublic:              append(ed25519.PublicKey(nil), value.RootPublic...),
		TrustedKeys:             cloneTrustedKeys(value.TrustedKeys),
		LocalKeys:               cloneKeys(value.LocalKeys),
		LocalCertificate:        value.LocalCertificate,
		LocalCertificateRaw:     append([]byte(nil), value.LocalCertificateRaw...),
		MachineCertificate:      value.MachineCertificate,
		MachineCertificateKeyID: value.MachineCertificateKeyID,
		MachineCertificateRaw:   append([]byte(nil), value.MachineCertificateRaw...),
	}
}

func cloneTrustedKeys(values []endpointidentity.TrustedKey) []endpointidentity.TrustedKey {
	result := make([]endpointidentity.TrustedKey, len(values))
	for i, value := range values {
		result[i] = value
		result[i].PublicKey = append([]byte(nil), value.PublicKey...)
	}
	return result
}

func cloneKeys(value config.PeerIdentityKeys) config.PeerIdentityKeys {
	value.RootPrivate = append([]byte(nil), value.RootPrivate...)
	value.QUICPrivate = append([]byte(nil), value.QUICPrivate...)
	return value
}
