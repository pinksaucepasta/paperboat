package native

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat-relay/reattach"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
)

type retiredPeerClientError struct{ error }

func (e *retiredPeerClientError) Unwrap() error { return e.error }

// Dial obtains a new QUIC association before any application request is sent.
// Recovering an idle route cannot replay exec, terminal input or transfer bytes.
func (o *Owner) Dial(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	var session *Session
	attempts := 0
	recovery := reattach.New(reattach.Policy{Attempts: 2, Retryable: func(err error) bool {
		var retired *retiredPeerClientError
		return errors.As(err, &retired)
	}})
	err := recovery.Run(ctx, func(ctx context.Context) error {
		if attempts > 0 {
			fresh, err := o.authority.Descriptor(peerID)
			if err != nil {
				return err
			}
			descriptor = fresh
		}
		attempts++
		var err error
		session, err = o.dialOnce(ctx, descriptor, peerID, class)
		return err
	})
	return session, err
}
