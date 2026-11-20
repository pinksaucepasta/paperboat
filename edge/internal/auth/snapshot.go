// Package auth verifies the signed credentials presented to the edge: Ed25519 signature against the
// issuer's key set, issuer, audience and node binding, lifetime with bounded clock skew, and
// revocation. It returns admission.Claims for a credential that passes and an error for any that does
// not; it never trusts a claim before the signature is checked.
package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/admission"
	"github.com/pinksaucepasta/paperboat/shared/jwks"
	"github.com/pinksaucepasta/paperboat/shared/strictjson"
)

const (
	maxSnapshotBytes     = 1 << 20
	maxSnapshotJSONDepth = 64
)

var (
	ErrSnapshotInvalid  = errors.New("credential trust snapshot is invalid")
	ErrSnapshotCapacity = errors.New("credential revocation watcher capacity exceeded")
)

const maximumRevocationWatchers = 8192

type Snapshot struct {
	mu                                         sync.RWMutex
	keys                                       map[string]ed25519.PublicKey
	trust                                      jwks.Trust // remembers withdrawn key ids, so a stale document never re-trusts one
	revokedJTI, revokedEnvironment, revokedKey map[string]struct{}
	revokedConnectorGeneration                 map[connectorGeneration]struct{}
	revocationMaxAge                           time.Duration
	revocationUpdatedAt                        time.Time
	now                                        func() time.Time
	watchers                                   map[*revocationOwner]revocationWatcher
	refresher                                  func(context.Context) error
	refreshedAt                                atomic.Int64 // unix nanos of the last kid-miss refresh
}

type revocationOwner struct {
	//lint:ignore U1000 The byte keeps independently allocated watcher owners pointer-distinct.
	marker byte
}
type revocationWatcher struct {
	claims admission.Claims
	done   chan struct{}
}
type connectorGeneration struct {
	Machine    string
	Connector  string
	Generation uint64
}

func NewSnapshot() *Snapshot {
	return &Snapshot{keys: map[string]ed25519.PublicKey{}, revokedJTI: map[string]struct{}{}, revokedEnvironment: map[string]struct{}{}, revokedKey: map[string]struct{}{}, revokedConnectorGeneration: map[connectorGeneration]struct{}{}, watchers: map[*revocationOwner]revocationWatcher{}, now: time.Now}
}

func (s *Snapshot) ConfigureRevocationFreshness(maxAge time.Duration, now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revocationMaxAge = maxAge
	if now != nil {
		s.now = now
	}
}

func (s *Snapshot) ReplaceJWKS(data []byte) error {
	next, err := s.trust.Apply(data)
	if err != nil {
		return fmt.Errorf("%w: apply signing keys; check the JWKS document: %w", ErrSnapshotInvalid, err)
	}
	s.mu.Lock()
	s.keys = next
	s.mu.Unlock()
	return nil
}

// keyRefreshInterval is the least time between kid-miss refreshes (cloudflared token/jwks.go jwksMinRefreshInterval: refetch once on a
// verify failure, at most once a minute; relay/derpquic/jwks.go does the same), so a rotated key is trusted at once, not at the next poll.
const keyRefreshInterval = time.Minute

// SetKeyRefresher installs the fetch-and-apply run when a token names a key id this snapshot does not hold.
func (s *Snapshot) SetKeyRefresher(refresh func(context.Context) error) {
	s.mu.Lock()
	s.refresher = refresh
	s.mu.Unlock()
}

func (s *Snapshot) Key(ctx context.Context, keyID string) (ed25519.PublicKey, error) {
	s.mu.RLock()
	key, refresh, now := s.keys[keyID], s.refresher, s.now()
	s.mu.RUnlock()
	if key == nil && refresh != nil {
		last := s.refreshedAt.Load()
		if now.UnixNano()-last >= int64(keyRefreshInterval) && s.refreshedAt.CompareAndSwap(last, now.UnixNano()) && refresh(ctx) == nil {
			s.mu.RLock()
			key = s.keys[keyID]
			s.mu.RUnlock()
		}
	}
	if key == nil {
		return nil, ErrSnapshotInvalid
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

type RevocationDocument struct {
	JTIs         []string                     `json:"jtis"`
	Environments []string                     `json:"environments"`
	Connectors   []RevokedConnectorGeneration `json:"connector_generations"`
	KeyIDs       []string                     `json:"key_ids"`
}
type RevokedConnectorGeneration struct {
	MachineID   string `json:"machine_id"`
	ConnectorID string `json:"connector_id"`
	Generation  uint64 `json:"connector_generation"`
}

func (s *Snapshot) ReplaceRevocations(data []byte) error {
	if len(data) == 0 || len(data) > maxSnapshotBytes {
		return ErrSnapshotInvalid
	}
	var document RevocationDocument
	if snapshotJSON(data, &document) != nil || len(document.JTIs)+len(document.Environments)+len(document.Connectors)+len(document.KeyIDs) > 10000 {
		return ErrSnapshotInvalid
	}
	jtis, environments, connectors, keys := map[string]struct{}{}, map[string]struct{}{}, map[connectorGeneration]struct{}{}, map[string]struct{}{}
	for _, value := range document.JTIs {
		if value == "" {
			return ErrSnapshotInvalid
		}
		jtis[value] = struct{}{}
	}
	for _, value := range document.Environments {
		if value == "" {
			return ErrSnapshotInvalid
		}
		environments[value] = struct{}{}
	}
	for _, value := range document.Connectors {
		if value.MachineID == "" || value.ConnectorID == "" || value.Generation == 0 {
			return ErrSnapshotInvalid
		}
		connectors[connectorGeneration{value.MachineID, value.ConnectorID, value.Generation}] = struct{}{}
	}
	for _, value := range document.KeyIDs {
		if value == "" {
			return ErrSnapshotInvalid
		}
		keys[value] = struct{}{}
	}
	s.mu.Lock()
	s.revokedJTI, s.revokedEnvironment, s.revokedConnectorGeneration, s.revokedKey = jtis, environments, connectors, keys
	clock := s.now
	if clock == nil {
		clock = time.Now
	}
	s.revocationUpdatedAt = clock()
	for id, watcher := range s.watchers {
		if s.revokedLocked(watcher.claims) {
			close(watcher.done)
			delete(s.watchers, id)
		}
	}
	s.mu.Unlock()
	return nil
}
func (s *Snapshot) Revoked(_ context.Context, claims admission.Claims) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.revocationsFreshLocked() {
		return false, ErrSnapshotInvalid
	}
	return s.revokedLocked(claims), nil
}

func (s *Snapshot) Watch(claims admission.Claims) (<-chan struct{}, func(), error) {
	if s == nil || claims.JTI == "" || claims.EnvironmentID == "" || claims.KeyID == "" {
		return nil, nil, ErrSnapshotInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.revocationsFreshLocked() {
		return nil, nil, ErrSnapshotInvalid
	}
	done := make(chan struct{})
	if s.revokedLocked(claims) {
		close(done)
		return done, func() {}, nil
	}
	if len(s.watchers) >= maximumRevocationWatchers {
		return nil, nil, ErrSnapshotCapacity
	}
	owner := &revocationOwner{}
	s.watchers[owner] = revocationWatcher{claims: claims, done: done}
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.watchers, owner)
			s.mu.Unlock()
		})
	}
	return done, release, nil
}

func (s *Snapshot) revocationsFreshLocked() bool {
	if s.revocationMaxAge <= 0 {
		return true
	}
	clock := s.now
	if clock == nil || s.revocationUpdatedAt.IsZero() {
		return false
	}
	now := clock()
	return !now.Before(s.revocationUpdatedAt) && now.Sub(s.revocationUpdatedAt) <= s.revocationMaxAge
}

func (s *Snapshot) revokedLocked(claims admission.Claims) bool {
	_, a := s.revokedJTI[claims.JTI]
	_, b := s.revokedEnvironment[claims.EnvironmentID]
	_, c := s.revokedConnectorGeneration[connectorGeneration{claims.MachineID, claims.ConnectorID, claims.ConnectorGeneration}]
	_, d := s.revokedKey[claims.KeyID]
	return a || b || c || d
}

// snapshotJSON ignores fields a newer server adds to a revocation document; a released edge must
// keep applying revocations from every later server (AGENTS.md). The document replaces every set, so
// a server keeps writing each revocation into these fields for every edge whose capability version
// (shared/capver) predates a new field: a revocation only in a new field would be lost here.
func snapshotJSON(data []byte, target any) error {
	if strictjson.DecodeResponse(data, target, maxSnapshotJSONDepth) != nil {
		return ErrSnapshotInvalid
	}
	return nil
}
