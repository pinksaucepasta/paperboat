package derpquic

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"time"
)

// errAuthorizeTransport marks a re-authentication that failed in the network, as opposed to one the relay refused
// or a grant that could not be encoded.
var errAuthorizeTransport = errors.New("re-authentication was not delivered")

// authorizeError classifies the failure of writing a re-authentication: a fatal carrier error (a grant too large to
// frame, ErrProtocol) comes from the grant, not the network, and stays itself so the carrier is revoked instead of
// retrying a send that can never succeed; anything else was lost in the network.
func authorizeError(err error) error {
	var fatal interface{ Fatal() bool }
	if err == nil || errors.As(err, &fatal) && fatal.Fatal() {
		return err
	}
	return fmt.Errorf("%w: %v", errAuthorizeTransport, err)
}

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
			if errors.Is(err, errAuthorizeTransport) {
				// The grant did not reach the relay: a network failure, not a refusal. Closing the connection
				// with ErrAdmission here latched the whole carrier fatal until a restart, so the grant is
				// sent again on the poll's own backoff and a dead connection ends by itself, redialled
				// without a latch.
				sent = ""
				return err
			}
			return admission.ErrRevoked // a grant this client cannot even encode
		}
		return nil
	})
	defer stopWatch()
	<-lease.Context().Done()
	if errors.Is(lease.Check(), admission.ErrRevoked) {
		revoke()
	}
}
