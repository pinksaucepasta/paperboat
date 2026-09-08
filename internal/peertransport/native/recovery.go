package native

import (
	"context"
	"errors"
	"fmt"

	"github.com/quic-go/quic-go"
)

// ReconnectRequiredError reports that an established peer QUIC connection was
// irreversibly lost. The application session manager owns obtaining fresh
// admission and resuming the application protocol; native transport never
// retries or replays a stream itself.
type ReconnectRequiredError struct {
	PeerID string
	Err    error
}

func (e *ReconnectRequiredError) Error() string {
	return fmt.Sprintf("peer %s connection lost; reconnect required: %v", e.PeerID, e.Err)
}

func (e *ReconnectRequiredError) Unwrap() error { return e.Err }

func reconnectRequired(peerID string, err error, locallyClosing bool) error {
	if err == nil || locallyClosing || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var streamErr *quic.StreamError
	if errors.As(err, &streamErr) {
		return err
	}
	var idleErr *quic.IdleTimeoutError
	var resetErr *quic.StatelessResetError
	if errors.As(err, &idleErr) || errors.As(err, &resetErr) || errors.Is(err, quic.ErrTransportClosed) {
		return &ReconnectRequiredError{PeerID: peerID, Err: err}
	}
	var applicationErr *quic.ApplicationError
	if errors.As(err, &applicationErr) && applicationErr.Remote && applicationErr.ErrorCode == 0 {
		return &ReconnectRequiredError{PeerID: peerID, Err: err}
	}
	// Transport errors include protocol and TLS/crypto violations. They are
	// deliberately terminal so a caller cannot turn them into a retry loop.
	return err
}
