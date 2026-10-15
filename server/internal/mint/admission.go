package mint

import (
	"errors"
	"time"
)

// ConfigureAdmissionLifetime is called once by server startup. It does not
// change credential-class validation bounds or identity certificate lifetimes.
func (p *Provider) ConfigureAdmissionLifetime(lifetime time.Duration) error {
	if p == nil || lifetime < time.Second || lifetime > MaxProofTTL {
		return errors.New("admission lifetime is outside proof bounds")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.admissionLifetime = lifetime
	return nil
}
func (p *Provider) AdmissionLifetime() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.admissionLifetime
}

// CredentialLifetimeLimit exposes the issuer's class-specific security bound.
func CredentialLifetimeLimit(class string) (time.Duration, bool) {
	policy, ok := credentialPolicies[class]
	return policy.maxTTL, ok
}
