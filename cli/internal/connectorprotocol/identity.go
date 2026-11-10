package connectorprotocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
)

func ValidateIdentifier(value string) error {
	if !identifierPattern.MatchString(value) || len(value) > MaxIdentifierBytes {
		return ErrInvalidInput
	}
	return nil
}

// IdentityThumbprint returns the RFC 7638-style SHA-256 thumbprint for an
// Ed25519 public key. The canonical JSON member order is part of the identity
// contract and matches the host enrollment store.
func IdentityThumbprint(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", ErrInvalidInput
	}
	canonical := `{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(publicKey) + `"}`
	digest := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}
