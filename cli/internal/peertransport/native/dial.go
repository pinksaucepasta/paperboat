package native

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat-relay/reattach"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/quic-go/quic-go"
)

// Dial obtains a new QUIC association before any application request is sent.
// Recovering a lost handshake cannot replay exec, terminal input or transfer bytes.
func (o *Owner) Dial(ctx context.Context, descriptor mesh.Addr, peerID string, class peerquic.Class) (*Session, error) {
	var session *Session
	attempts := 0
	recovery := reattach.New(reattach.Policy{Attempts: 2, Retryable: func(err error) bool {
		var failure *peerOperationFailure
		var idle *quic.IdleTimeoutError
		return errors.As(err, &failure) && failure.retryable && errors.As(err, &idle)
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
