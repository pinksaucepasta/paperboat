// Package jwks reads the mint trust document (signing keys plus withdrawn_key_ids) that relays and
// tunnel edges trust, with one strict parser for both.
package jwks

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"maps"
	"sync"

	"github.com/pinksaucepasta/paperboat/shared/strictjson"
)

const (
	// MaxKeys bounds the keys and the withdrawn ids of one document.
	MaxKeys  = 128
	MaxBytes = 128 << 10
	maxDepth = 64
	maxKeyID = 128
)

var ErrInvalid = errors.New("issuer JWKS is invalid")

type document struct {
	Keys            []jwk    `json:"keys"`
	WithdrawnKeyIDs []string `json:"withdrawn_key_ids,omitempty"`
}
type jwk struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	X         string `json:"x"`
}

// Trust applies trust documents and never re-trusts a key id once any document withdrew it. A rolling
// rotation behind several control-plane instances can serve new-then-old; without this the node would
// trust the withdrawn key again until its next poll (Teleport lib/auth/rotate.go: a rotation phase never
// goes back; TUF: a client never accepts older metadata). The zero value is ready; the set lives as long
// as the Trust, one process, and is bounded.
type Trust struct {
	mu        sync.Mutex
	withdrawn map[string]struct{}
}

// maxWithdrawn bounds the remembered ids: one document names at most MaxKeys, and an operator revokes a
// signing key only rarely.
const maxWithdrawn = 1024

// Apply reads a document like Parse and remembers its withdrawn ids; a refused document remembers nothing.
func (t *Trust) Apply(raw []byte) (map[string]ed25519.PublicKey, error) {
	keys, withdrawn, err := decode(raw)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	next := maps.Clone(t.withdrawn)
	if next == nil {
		next = map[string]struct{}{}
	}
	for _, id := range withdrawn {
		next[id] = struct{}{}
	}
	if len(next) > maxWithdrawn {
		return nil, ErrInvalid
	}
	for id := range next {
		delete(keys, id)
	}
	if len(keys) == 0 {
		return nil, ErrInvalid
	}
	t.withdrawn = next
	return keys, nil
}

// Parse reads one document on its own, without memory of earlier ones.
func Parse(raw []byte) (map[string]ed25519.PublicKey, error) { return new(Trust).Apply(raw) }

// decode validates the document and returns its listed keys and its withdrawn ids.
func decode(raw []byte) (map[string]ed25519.PublicKey, []string, error) {
	if len(raw) == 0 || len(raw) > MaxBytes {
		return nil, nil, ErrInvalid
	}
	var doc document
	if strictjson.Decode(raw, &doc, maxDepth) != nil || len(doc.Keys) == 0 || len(doc.Keys) > MaxKeys || len(doc.WithdrawnKeyIDs) > MaxKeys {
		return nil, nil, ErrInvalid
	}
	keys := make(map[string]ed25519.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.KeyType != "OKP" || k.Curve != "Ed25519" || k.Use != "sig" || k.Algorithm != "EdDSA" || k.KeyID == "" || len(k.KeyID) > maxKeyID {
			return nil, nil, ErrInvalid
		}
		b, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(b) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(b) != k.X || keys[k.KeyID] != nil {
			return nil, nil, ErrInvalid
		}
		keys[k.KeyID] = ed25519.PublicKey(b)
	}
	for _, id := range doc.WithdrawnKeyIDs {
		if id == "" || len(id) > maxKeyID {
			return nil, nil, ErrInvalid
		}
	}
	return keys, doc.WithdrawnKeyIDs, nil
}
