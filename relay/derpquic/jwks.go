package derpquic

import (
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

var errUnknownKey = errors.New("unknown grant key id")

func copyKeys(keys map[string]ed25519.PublicKey) (map[string]ed25519.PublicKey, error) {
	if len(keys) == 0 {
		return nil, ErrAdmission
	}
	out := make(map[string]ed25519.PublicKey, len(keys))
	for id, k := range keys {
		if id == "" || len(k) != ed25519.PublicKeySize {
			return nil, ErrAdmission
		}
		out[id] = append(ed25519.PublicKey(nil), k...)
	}
	return out, nil
}

// SetKeys replaces the trusted grant keys (copy on write: grants being verified keep the set they started with).
func (s *Server) SetKeys(keys map[string]ed25519.PublicKey) error {
	next, err := copyKeys(keys)
	if err != nil {
		return err
	}
	s.keys.Store(&next)
	return nil
}

// SetKeyRefresher installs the out-of-band refresh run when a grant names a key id this relay does not
// trust (cloudflared token/jwks.go refetches once on a verify failure, at most once a minute).
func (s *Server) SetKeyRefresher(refresh func() error) { s.refresher.Store(&refresh) }

const keyRefreshInterval = time.Minute

// verify checks a grant against the current keys; an unknown key id triggers one rate-limited refresh and a retry.
func (s *Server) verify(token string, state tls.ConnectionState, now time.Time) (Grant, error) {
	v := s.verifier
	v.Keys = *s.keys.Load()
	grant, err := v.Verify(token, state, now)
	if !errors.Is(err, errUnknownKey) {
		return grant, err
	}
	refresh := s.refresher.Load()
	if refresh == nil {
		return grant, err
	}
	if !s.refreshGate(now) {
		return Grant{}, ErrExpired
	}
	if err := (*refresh)(); err != nil {
		return Grant{}, fmt.Errorf("refresh grant signing keys: %w", errors.Join(ErrExpired, err))
	}
	v.Keys = *s.keys.Load()
	return v.Verify(token, state, now)
}

func (s *Server) refreshGate(now time.Time) bool {
	last := s.keysRefreshedAt.Load()
	return now.UnixNano()-last >= int64(keyRefreshInterval) && s.keysRefreshedAt.CompareAndSwap(last, now.UnixNano())
}

// KeyIDs lists the trusted grant key ids, sorted.
func (s *Server) KeyIDs() []string {
	return slices.Sorted(maps.Keys(*s.keys.Load()))
}
