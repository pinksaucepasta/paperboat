package derpquic

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-relay/admission"
	"time"
)

// The carrier has already authenticated. Refresh publishes policy changes; a
// credential's TTL or a failed refresh cannot revoke its admitted lifetime.
// Only a changed grant is sent: the relay verifies each one (Ed25519 and X.509
// under its lock), and re-sending an unchanged grant every poll cost it one
// verification per connection per second, which stalled admissions near 1,000
// connections.
func watchCarrierAuthority(ctx context.Context, credential func(context.Context) (string, error), send func(string) error, revoke func()) {
	lease, err := admission.Admit(ctx, time.Time{}, nil)
	if err != nil {
		return
	}
	defer lease.Close()
	sent := "" // the connection's admission grant counts as unsent: a first poll publishes a changed one
	stopWatch := lease.Poll(func(parent context.Context) error {
		token, err := credential(parent)
		if errors.Is(err, ErrAdmission) {
			return admission.ErrRevoked
		}
		if err != nil || token == "" || token == sent {
			return nil
		}
		sent = token
		if err := send(token); err != nil {
			return admission.ErrRevoked
		}
		return nil
	})
	defer stopWatch()
	<-lease.Context().Done()
	if errors.Is(lease.Check(), admission.ErrRevoked) {
		revoke()
	}
}
