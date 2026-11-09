// Package jwks reads the mint trust document (signing keys plus withdrawn_key_ids) that relays and
// tunnel edges trust, with one strict parser for both.
package jwks

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"

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

// Parse reads the trust document into the grant verification keys. Keys named in withdrawn_key_ids are
// dropped even while listed. A malformed document, or one that leaves no key, is refused so a caller
// keeps its last good set instead of trusting nothing.
func Parse(raw []byte) (map[string]ed25519.PublicKey, error) {
	if len(raw) == 0 || len(raw) > MaxBytes {
		return nil, ErrInvalid
	}
	var doc document
	if strictjson.Decode(raw, &doc, maxDepth) != nil || len(doc.Keys) == 0 || len(doc.Keys) > MaxKeys || len(doc.WithdrawnKeyIDs) > MaxKeys {
		return nil, ErrInvalid
	}
	keys := make(map[string]ed25519.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.KeyType != "OKP" || k.Curve != "Ed25519" || k.Use != "sig" || k.Algorithm != "EdDSA" || k.KeyID == "" || len(k.KeyID) > maxKeyID {
			return nil, ErrInvalid
		}
		b, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(b) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(b) != k.X || keys[k.KeyID] != nil {
			return nil, ErrInvalid
		}
		keys[k.KeyID] = ed25519.PublicKey(b)
	}
	for _, id := range doc.WithdrawnKeyIDs {
		if id == "" || len(id) > maxKeyID {
			return nil, ErrInvalid
		}
		delete(keys, id)
	}
	if len(keys) == 0 {
		return nil, ErrInvalid
	}
	return keys, nil
}
