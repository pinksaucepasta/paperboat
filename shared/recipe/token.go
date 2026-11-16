package recipe

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
)

// Token shape: "pbr_" + 10-character id + "_" + 52-character secret (32 random bytes), lowercase
// base32, so secret scanners can find a leaked one (PLAN F26 section 6).
const (
	TokenPrefix  = "pbr_"
	idLength     = 10
	secretLength = 52
	TokenLength  = len(TokenPrefix) + idLength + 1 + secretLength
	secretBytes  = 32
)

var encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Token is a parsed recipe token. ID is public (it names the recipe's token and survives
// rotation); Secret never leaves the machine except as Auth.
type Token struct {
	ID     string
	Secret []byte
}

func (t Token) String() string { return TokenPrefix + t.ID + "_" + encoding.EncodeToString(t.Secret) }

// Masked is the token as list and show print it: "pbr_<id>_…".
func (t Token) Masked() string { return MaskedID(t.ID) }

// MaskedID prints a token id the way list and show do.
func MaskedID(id string) string { return TokenPrefix + id + "_…" }

// NewID returns a fresh public token id.
func NewID() (string, error) {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return encoding.EncodeToString(b)[:idLength], nil
}

// NewToken returns a token with a fresh secret for id ("" makes a new id).
func NewToken(id string) (Token, error) {
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return Token{}, err
		}
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return Token{}, err
	}
	return Token{ID: id, Secret: secret}, nil
}

// ShapeError is a value that is not a recipe token; Length is what was given.
type ShapeError struct{ Length int }

func (e *ShapeError) Error() string {
	return fmt.Sprintf("this is not a recipe token. A token starts with %s and is %d characters; this value is %d.", TokenPrefix, TokenLength, e.Length)
}

// ParseToken reads "pbr_<id>_<secret>"; surrounding white space is ignored.
func ParseToken(s string) (Token, error) {
	s = strings.TrimSpace(s)
	bad := &ShapeError{Length: len(s)}
	rest, ok := strings.CutPrefix(s, TokenPrefix)
	if !ok || len(s) != TokenLength || rest[idLength] != '_' {
		return Token{}, bad
	}
	id, secretText := rest[:idLength], rest[idLength+1:]
	if !ValidID(id) {
		return Token{}, bad
	}
	secret, err := encoding.DecodeString(secretText)
	if err != nil || len(secret) != secretBytes {
		return Token{}, bad
	}
	return Token{ID: id, Secret: secret}, nil
}

// ValidID reports whether id has the shape of a token id.
func ValidID(id string) bool {
	if len(id) != idLength {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz234567", r) {
			return false
		}
	}
	return true
}

func derive(secret []byte, info string) []byte {
	out, err := hkdf.Key(sha256.New, secret, nil, info, 32)
	if err != nil {
		panic(err) // HKDF-SHA256 yields far more than 32 bytes
	}
	return out
}

// Auth is the value the machine sends to the server; the server stores only AuthHash(Auth).
func (t Token) Auth() []byte { return derive(t.Secret, "paperboat recipe auth") }

// AuthHash is what the server stores and looks up.
func AuthHash(auth []byte) []byte {
	sum := sha256.Sum256(auth)
	return sum[:]
}

// PrivateKey is the recipe's X25519 private key; it never leaves the machine. Vault keys are
// wrapped to PublicKey, so the server can admit a machine but never decrypt a vault.
func (t Token) PrivateKey() []byte { return derive(t.Secret, "paperboat recipe key") }

// PublicKey is the X25519 public key of PrivateKey.
func (t Token) PublicKey() []byte {
	key, err := ecdh.X25519().NewPrivateKey(t.PrivateKey())
	if err != nil {
		panic(err) // any 32 bytes are an X25519 private key
	}
	return key.PublicKey().Bytes()
}
