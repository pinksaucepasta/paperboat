// Package secrets encrypts and decrypts stored secrets with AES-GCM under a key derived from the
// configured key material. Decrypt wraps every failure (wrong key, tampered data) in ErrDecrypt, and
// the byte-slice variants return caller-owned plaintext so sensitive callers can clear it.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrDecrypt wraps every failure to decrypt a stored secret (wrong key, tampered
// or truncated ciphertext). Callers can use errors.Is to detect it — e.g. so
// project teardown can proceed even when a secret was encrypted under a key that
// is no longer configured.
var ErrDecrypt = errors.New("secret decryption failed")

// keySeparator splits a configured key into its ring. ValidRing refuses a ring with an empty key, so a stray comma
// cannot make the newest key the empty one.
const keySeparator = ","

// stamp marks a ciphertext that carries the id of the key that sealed it: stamp, 8-byte key id, nonce, sealed.
var stamp = []byte("pbk1")

const keyIDSize = 8

// Ring returns the keys of a configured key, newest first.
func Ring(key []byte) [][]byte { return bytes.Split(key, []byte(keySeparator)) }

// KeyID names a key without revealing it; it is stored with every ciphertext the key seals.
func KeyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("paperboat-secret-key-id:"), key...))
	return hex.EncodeToString(sum[:keyIDSize])
}

// KeyIDs are the ids of the ring, newest first.
func KeyIDs(key string) []string {
	ring := Ring([]byte(key))
	ids := make([]string, len(ring))
	for index, one := range ring {
		ids[index] = KeyID(one)
	}
	return ids
}

// ValidRing says every key of a configured ring is non-empty. A key that itself contains the separator is
// indistinguishable from two keys: keys are random strings, not text.
func ValidRing(key string) bool {
	for _, one := range Split(key) {
		if one == "" {
			return false
		}
	}
	return true
}

// Split returns each key of a configured key, for validation.
func Split(key string) []string { return strings.Split(key, keySeparator) }

func Encrypt(key string, plaintext string) ([]byte, error) {
	keyBytes := []byte(key)
	plaintextBytes := []byte(plaintext)
	defer clear(keyBytes)
	defer clear(plaintextBytes)
	return EncryptBytes(keyBytes, plaintextBytes)
}

// EncryptBytes encrypts secret material with the newest key of the ring without requiring callers to create
// immutable string copies. The input slices remain owned by the caller and are
// not modified. Callers should clear them as soon as they are no longer needed.
func EncryptBytes(key, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, errors.New("cannot encrypt empty secret")
	}
	newest := Ring(key)[0]
	gcm, err := gcmFor(newest)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append([]byte(nil), stamp...), mustDecodeID(KeyID(newest))...)
	return append(append(out, nonce...), gcm.Seal(nil, nonce, plaintext, nil)...), nil
}

func mustDecodeID(id string) []byte {
	raw, _ := hex.DecodeString(id)
	return raw
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	sum := sha256.Sum256(key)
	defer clear(sum[:])
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Decrypt(key string, ciphertext []byte) (string, error) {
	keyBytes := []byte(key)
	defer clear(keyBytes)
	plaintext, err := DecryptBytes(keyBytes, ciphertext)
	if err != nil {
		return "", err
	}
	result := string(plaintext)
	clear(plaintext)
	return result, nil
}

// stampedKeyID is the key id a ciphertext names, if it carries one.
func stampedKeyID(ciphertext []byte) (string, []byte, bool) {
	if len(ciphertext) < len(stamp)+keyIDSize || !bytes.HasPrefix(ciphertext, stamp) {
		return "", ciphertext, false
	}
	return hex.EncodeToString(ciphertext[len(stamp) : len(stamp)+keyIDSize]), ciphertext[len(stamp)+keyIDSize:], true
}

// DecryptBytes returns caller-owned plaintext bytes so security-sensitive
// callers can clear them after use. It opens the ciphertext with the key it names, then with the rest of the
// ring (a ciphertext without a stamp predates key ids), and wraps every failure (wrong key, malformed or
// tampered ciphertext) in ErrDecrypt.
func DecryptBytes(key, ciphertext []byte) ([]byte, error) {
	named, sealed, stamped := stampedKeyID(ciphertext)
	ring := Ring(key)
	if stamped {
		slices.SortStableFunc(ring, func(a, b []byte) int { return boolInt(KeyID(b) == named) - boolInt(KeyID(a) == named) })
	}
	var last error
	for _, one := range ring {
		plaintext, err := open(one, sealed)
		if err == nil {
			return plaintext, nil
		}
		last = err
	}
	return nil, last
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func open(key, sealed []byte) ([]byte, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("%w: ciphertext too short", ErrDecrypt)
	}
	nonce, encrypted := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}
	return plaintext, nil
}

// NeedsReseal says whether a ciphertext is not yet sealed under the newest key of the ring, without opening it.
func NeedsReseal(key string, ciphertext []byte) bool {
	named, _, stamped := stampedKeyID(ciphertext)
	return !stamped || named != KeyIDs(key)[0]
}

// Reseal opens a ciphertext with any key of the ring and seals it again under the newest key.
func Reseal(key string, ciphertext []byte) ([]byte, error) {
	keyBytes := []byte(key)
	defer clear(keyBytes)
	plaintext, err := DecryptBytes(keyBytes, ciphertext)
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	return EncryptBytes(keyBytes, plaintext)
}

// Stamp is the prefix every ciphertext sealed under the newest key of the ring starts with: SQL can find the rest
// (`substring(col from 1 for 12) <> $1`) without opening anything.
func Stamp(key string) []byte {
	return append(append([]byte(nil), stamp...), mustDecodeID(KeyIDs(key)[0])...)
}
