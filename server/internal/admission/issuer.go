// Package admission owns short-lived credential issuance. Expiry authorizes
// admission only; consumers retain admitted work until disconnect or revocation.
package admission

import (
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"time"
)

const DefaultLifetime = 2 * time.Minute

type Signer interface {
	SignCredential(mint.CredentialInput) (string, error)
}

type Issuer struct {
	signer   Signer
	lifetime time.Duration
}

type Credential struct {
	Token               string
	IssuedAt, ExpiresAt time.Time
}

func New(signer Signer, lifetime time.Duration) (*Issuer, error) {
	if lifetime == 0 {
		lifetime = Lifetime(signer)
	}
	if signer == nil || lifetime < time.Second || lifetime > mint.MaxProofTTL {
		return nil, errors.New("admission issuer requires a signer and lifetime between one second and maximum proof TTL")
	}
	return &Issuer{signer: signer, lifetime: lifetime}, nil
}

// Expiry returns the same second-granular deadline used in signed claims.
func (i *Issuer) Expiry(issuedAt time.Time) time.Time {
	return Deadline(issuedAt, i.lifetime)
}

// Issue applies the one configured lifetime, optionally capped by an earlier
// resource deadline. Callers use the returned deadline in their descriptor.
func (i *Issuer) Issue(input mint.CredentialInput) (Credential, error) {
	if i == nil || input.IssuedAt.IsZero() {
		return Credential{}, errors.New("admission issue time is required")
	}
	input.IssuedAt = input.IssuedAt.UTC().Truncate(time.Second)
	expiry := i.Expiry(input.IssuedAt)
	if maximum, ok := mint.CredentialLifetimeLimit(input.CredentialClass); ok && expiry.Sub(input.IssuedAt) > maximum {
		expiry = input.IssuedAt.Add(maximum)
	}
	if !input.ExpiresAt.IsZero() && input.ExpiresAt.Before(expiry) {
		expiry = input.ExpiresAt.UTC().Truncate(time.Second)
	}
	input.ExpiresAt = expiry
	token, err := i.signer.SignCredential(input)
	if err != nil {
		return Credential{}, err
	}
	return Credential{Token: token, IssuedAt: input.IssuedAt, ExpiresAt: expiry}, nil
}

// Lifetime reads the configured provider policy; lightweight test/external
// signers share the same default. Configuration validation owns its bounds.
func Lifetime(signer any) time.Duration {
	if p, ok := signer.(interface{ AdmissionLifetime() time.Duration }); ok && p.AdmissionLifetime() > 0 {
		return p.AdmissionLifetime()
	}
	return DefaultLifetime
}
func Expiry(signer any, issuedAt time.Time) time.Time {
	return Deadline(issuedAt, Lifetime(signer))
}

// Deadline applies the configured lifetime to server-issued admission metadata.
func Deadline(issuedAt time.Time, lifetime time.Duration) time.Time {
	if lifetime == 0 {
		lifetime = DefaultLifetime
	}
	return issuedAt.UTC().Truncate(time.Second).Add(lifetime).Truncate(time.Second)
}

// Sign is for descriptors that already obtain their deadline from Expiry.
func Sign(signer Signer, input mint.CredentialInput) (string, error) {
	issuer, err := New(signer, 0)
	if err != nil {
		return "", err
	}
	credential, err := issuer.Issue(input)
	return credential.Token, err
}
