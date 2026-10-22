package endpointidentity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// TrustedKey is public account trust metadata for one independently enrolled
// endpoint signing key. The key itself is never private key custody.
type TrustedKey struct {
	KeyID       string
	PublicKey   ed25519.PublicKey
	Fingerprint [sha256.Size]byte
	Generation  uint64
}

// The account trust set holds one key per enrolled machine and one per signed-in CLI session (every paired
// computer has both), and plans do not limit machines. MaxTrustedKeys is the stated product maximum: 1024
// computers with their pairing sessions. Every store and response that carries the set sizes itself from
// these two values, so the set cannot be accepted by one step and refused by the next.
const (
	MaxTrustedKeys = 2048
	// MaxTrustedKeyBytes bounds the JSON encoding of one key (id, public key, fingerprint, generation and
	// field names); real keys encode to about 260 bytes.
	MaxTrustedKeyBytes = 384
	// MaxTrustedKeySetBytes bounds the JSON encoding of a full set.
	MaxTrustedKeySetBytes = MaxTrustedKeys * MaxTrustedKeyBytes
)

var ErrTrustedKeySet = errors.New("trusted E2EE key set is invalid")

// ValidateTrustedKeySet checks the complete active trust set and returns a
// private copy suitable for retaining in a runtime authority. A set is
// intentionally required to contain at least one key and may not contain
// duplicate key IDs, public keys, or fingerprints.
func ValidateTrustedKeySet(keys []TrustedKey) ([]TrustedKey, error) {
	if len(keys) == 0 {
		return nil, ErrTrustedKeySet
	}
	if len(keys) > MaxTrustedKeys {
		return nil, fmt.Errorf("%w: the account has %d active keys, more than the %d supported", ErrTrustedKeySet, len(keys), MaxTrustedKeys)
	}
	result := make([]TrustedKey, 0, len(keys))
	seenIDs := make(map[string]struct{}, len(keys))
	seenFingerprints := make(map[[sha256.Size]byte]struct{}, len(keys))
	for _, key := range keys {
		if !ValidIdentifier(key.KeyID) || len(key.PublicKey) != ed25519.PublicKeySize || key.Generation == 0 {
			return nil, ErrTrustedKeySet
		}
		fingerprint := sha256.Sum256(key.PublicKey)
		if key.Fingerprint != fingerprint {
			return nil, ErrTrustedKeySet
		}
		if _, ok := seenIDs[key.KeyID]; ok {
			return nil, ErrTrustedKeySet
		}
		if _, ok := seenFingerprints[key.Fingerprint]; ok {
			return nil, ErrTrustedKeySet
		}
		seenIDs[key.KeyID] = struct{}{}
		seenFingerprints[key.Fingerprint] = struct{}{}
		result = append(result, TrustedKey{KeyID: key.KeyID, PublicKey: append(ed25519.PublicKey(nil), key.PublicKey...), Fingerprint: key.Fingerprint, Generation: key.Generation})
	}
	return result, nil
}

func TrustedKeyFor(keys []TrustedKey, keyID string) (TrustedKey, bool) {
	for _, key := range keys {
		if key.KeyID == keyID {
			return TrustedKey{KeyID: key.KeyID, PublicKey: append(ed25519.PublicKey(nil), key.PublicKey...), Fingerprint: key.Fingerprint, Generation: key.Generation}, true
		}
	}
	return TrustedKey{}, false
}

// VerifyWithTrustedKey verifies an endpoint certificate against the key named
// by its API metadata. The key ID is deliberately required because the same
// account may have several active signing keys.
func VerifyWithTrustedKey(raw []byte, keyID string, keys []TrustedKey, expected Expected, now time.Time) (Certificate, error) {
	key, ok := TrustedKeyFor(keys, keyID)
	if !ok {
		return Certificate{}, ErrTrustedKeySet
	}
	certificate, err := Verify(raw, key.PublicKey, expected, now)
	if err != nil {
		return Certificate{}, err
	}
	return certificate, nil
}

func TrustedKeyFingerprint(key TrustedKey) string {
	if key.Fingerprint == ([sha256.Size]byte{}) {
		return ""
	}
	return hex.EncodeToString(key.Fingerprint[:])
}

func TrustedKeyMatchesPublic(key TrustedKey, public ed25519.PublicKey) bool {
	return len(public) == ed25519.PublicKeySize && bytes.Equal(key.PublicKey, public)
}
