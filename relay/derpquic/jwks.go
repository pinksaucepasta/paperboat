package derpquic

import (
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"
)

// MaxJWKSKeys bounds the trust document, in step with the tunnel edge's snapshot (tunnel/internal/auth).
const MaxJWKSKeys = 128

var errUnknownKey = errors.New("unknown grant key id")

// ParseJWKS reads the issuer trust document into the grant verification keys. Keys named in
// withdrawn_key_ids are dropped even while listed; a document that leaves no key is refused, so a
// malformed one never makes the relay trust nothing.
func ParseJWKS(raw []byte) (map[string]ed25519.PublicKey, error) {
	var document struct {
		Keys            []struct{ Kty, Crv, Kid, X string } `json:"keys"`
		WithdrawnKeyIDs []string                            `json:"withdrawn_key_ids"`
	}
	if json.Unmarshal(raw, &document) != nil || len(document.Keys) == 0 || len(document.Keys) > MaxJWKSKeys || len(document.WithdrawnKeyIDs) > MaxJWKSKeys {
		return nil, errors.New("invalid issuer JWKS")
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, k := range document.Keys {
		b, err := base64.RawURLEncoding.Strict().DecodeString(k.X)
		if err != nil || k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" || len(b) != ed25519.PublicKeySize || keys[k.Kid] != nil {
			return nil, errors.New("invalid issuer signing key")
		}
		keys[k.Kid] = ed25519.PublicKey(b)
	}
	for _, id := range document.WithdrawnKeyIDs {
		if id == "" {
			return nil, errors.New("invalid withdrawn issuer key id")
		}
		delete(keys, id)
	}
	if len(keys) == 0 {
		return nil, errors.New("issuer JWKS withdraws every key")
	}
	return keys, nil
}

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
	if refresh == nil || !s.refreshGate(now) || (*refresh)() != nil {
		return grant, err
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
