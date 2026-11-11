package connectorprotocol

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"regexp"
	"strings"
)

const originCABlockStart = "-----BEGIN CERTIFICATE-----"

// originTLSPinPattern is the form of a pinned route's certificate pin: sha256:<hex>.
var originTLSPinPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// MaximumOriginCABytes bounds the CA a route trusts for its https origin (a root plus intermediates).
const MaximumOriginCABytes = 16 << 10

// ErrOriginCAInvalid: the route CA is not a bounded PEM list of CA certificates.
var ErrOriginCAInvalid = errors.New("the origin CA must be PEM CA certificates (at most 8, 16 KiB)")

// ValidateOriginCA accepts what cloudflared's caPool accepts (tlsconfig/origin_ca.go), bounded: one to
// eight PEM CERTIFICATE blocks, each a CA, and nothing else (no keys, no trailing text).
func ValidateOriginCA(value string) error {
	if value == "" || len(value) > MaximumOriginCABytes {
		return ErrOriginCAInvalid
	}
	rest, count := []byte(value), 0
	for {
		// pem.Decode skips any text before the first block; the CA is PEM certificates and nothing else.
		if trimmed := strings.TrimSpace(string(rest)); trimmed == "" {
			break
		} else if !strings.HasPrefix(trimmed, originCABlockStart) {
			return ErrOriginCAInvalid
		}
		block, remaining := pem.Decode(rest)
		if block == nil {
			return ErrOriginCAInvalid
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if block.Type != "CERTIFICATE" || err != nil || !certificate.IsCA {
			return ErrOriginCAInvalid
		}
		count, rest = count+1, remaining
	}
	if count == 0 || count > 8 {
		return ErrOriginCAInvalid
	}
	return nil
}

// OriginCAFingerprint is how a signed ingress binding names a route's CA: the hex SHA-256 of its PEM.
func OriginCAFingerprint(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
