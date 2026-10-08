// Package nodetls verifies TLS identities published in signed node discovery.
package nodetls

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
)

var ErrIdentity = errors.New("node TLS identity is invalid")

// ParsePin accepts only a canonical base64url SHA-256 SPKI digest.
func ParsePin(pin string) ([]byte, error) {
	digest, err := base64.RawURLEncoding.Strict().DecodeString(pin)
	if err != nil || len(digest) != sha256.Size || base64.RawURLEncoding.EncodeToString(digest) != pin {
		return nil, ErrIdentity
	}
	return digest, nil
}

// ClientConfig preserves the client's credentials and uses a signed SPKI pin
// as the server trust anchor. Empty pins retain ordinary hosted-node PKI.
// Pinned identities still require a valid certificate and server-auth usage.
func ClientConfig(base *tls.Config, pin string) (*tls.Config, error) {
	if base == nil {
		return nil, ErrIdentity
	}
	result := base.Clone()
	if pin == "" {
		return result, nil
	}
	digest, err := ParsePin(pin)
	if err != nil {
		return nil, err
	}
	previous := result.VerifyConnection
	result.InsecureSkipVerify = true // Verified below using the signed identity.
	result.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return ErrIdentity
		}
		leaf := state.PeerCertificates[0]
		actual := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(actual[:], digest) != 1 {
			return ErrIdentity
		}
		roots := x509.NewCertPool()
		roots.AddCert(leaf)
		now := base.Time
		options := x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if now != nil {
			options.CurrentTime = now()
		}
		if _, err := leaf.Verify(options); err != nil {
			return identityFailure{cause: err}
		}
		if previous != nil {
			return previous(state)
		}
		return nil
	}
	return result, nil
}

// identityFailure preserves the typed verification cause without exposing certificate details.
type identityFailure struct{ cause error }

func (identityFailure) Error() string     { return ErrIdentity.Error() }
func (e identityFailure) Unwrap() []error { return []error{ErrIdentity, e.cause} }
