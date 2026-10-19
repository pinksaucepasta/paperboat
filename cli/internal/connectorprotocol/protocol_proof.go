package connectorprotocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
)

func ValidateIdentifier(value string) error {
	if !identifierPattern.MatchString(value) || len(value) > MaxIdentifierBytes {
		return ErrInvalidInput
	}
	return nil
}

func ValidateCredentialReference(value string) error {
	if len(value) == 0 || len(value) > MaxCredentialRef || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return ErrInvalidInput
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host != "paperboat" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" {
		return ErrInvalidInput
	}
	switch parsed.Scheme {
	case "keychain", "credential-manager", "secret-service", "protected-file", "tpm":
		return nil
	default:
		return ErrInvalidInput
	}
}

func ValidateProof(value string) error {
	if len(value) < 32 || len(value) > MaxProofBytes || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return ErrInvalidInput
	}
	return nil
}

// VerifyProofFunc lets the host identity package keep ownership of public-key
// lookup and revocation while this package owns the signed transcript.
type VerifyProofFunc func(message, signature []byte) bool

func SignProofPayload(payload []byte, sign func([]byte) []byte) (string, error) {
	if len(payload) == 0 || len(payload) > MaxProofBytes || sign == nil {
		return "", ErrInvalidInput
	}
	signature := sign(payload)
	if len(signature) == 0 || len(signature) > MaxProofBytes {
		return "", ErrInvalidInput
	}
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyProofPayload(payload []byte, signedProof string, verify VerifyProofFunc) error {
	if len(payload) == 0 || verify == nil {
		return ErrAuthenticationFailed
	}
	signature, err := DecodeProof(signedProof)
	if err != nil || !verify(payload, signature) {
		return ErrAuthenticationFailed
	}
	return nil
}

// DecodeProof decodes the detached base64url signature without exposing any
// credential material. Server adapters use this after deriving the exact
// transcript and resolving the enrolled public identity for the request.
func DecodeProof(signedProof string) ([]byte, error) {
	if ValidateProof(signedProof) != nil {
		return nil, ErrAuthenticationFailed
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(signedProof)
	if err != nil || len(signature) == 0 || len(signature) > MaxProofBytes {
		return nil, ErrAuthenticationFailed
	}
	return signature, nil
}

// ValidateIdentityKey checks the stable public-key identity metadata used by
// the host enrollment store. The private key, certificate bytes, and bearer
// credential never cross this protocol boundary.
func ValidateIdentityKey(keyID, thumbprint string) error {
	if !identityKeyIDPattern.MatchString(keyID) || !identityThumbprintPattern.MatchString(thumbprint) || keyID[len("ed25519:"):] != thumbprint {
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

func IdentityKeyID(publicKey ed25519.PublicKey) (string, error) {
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil {
		return "", err
	}
	return "ed25519:" + thumbprint, nil
}
