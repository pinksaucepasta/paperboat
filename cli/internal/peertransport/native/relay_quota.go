package native

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// RelayQuotaError is a failed peer connection made while the workspace's hosted relay allowance is
// used up. Direct connections are unaffected by the allowance, so a connection that needed a relay
// fails for that reason; the error carries the plan-limit message and upgrade action instead of a
// generic transport failure. It still unwraps to the transport failure it replaces.
type RelayQuotaError struct {
	Denial *api.APIError
	Err    error
}

func (e *RelayQuotaError) Error() string           { return e.Denial.Message }
func (e *RelayQuotaError) Unwrap() error           { return e.Err }
func (e *RelayQuotaError) LocalAPICode() string    { return e.Denial.Code }
func (e *RelayQuotaError) LocalAPIMessage() string { return e.Denial.Message }

type relayDenier interface{ RelayDenial() *api.APIError }

// relayQuotaFailure marks a connection failure as caused by an exhausted relay allowance when the
// latest network refresh withheld relay grants. Only a failure to connect qualifies: authority and
// admission rejections have their own meaning and cancellation is the caller's.
func relayQuotaFailure(denier relayDenier, err error) error {
	var failure *peerOperationFailure
	connect := errors.As(err, &failure) && failure.stage == "peer_connect"
	if err == nil || denier == nil || !connect && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if denial := denier.RelayDenial(); denial != nil {
		return &RelayQuotaError{Denial: denial, Err: err}
	}
	return err
}

// relayAnswerMargin is kept before the caller's deadline when relays are withheld, so the dial's own
// failure is reported (as the plan limit) before the caller times out.
const relayAnswerMargin = 2 * time.Second

// relayWithheldDialTimeout bounds a dial without a caller deadline (a file transfer) while relays are
// withheld at the plan limit: a peer that needs a relay is never reached, and waiting for it made pb send
// hang for minutes and end "canceled" instead of naming the limit (Lane O, F13-03S).
const relayWithheldDialTimeout = 15 * time.Second

func relayWithheldDeadline(ctx context.Context, denier relayDenier) (context.Context, context.CancelFunc) {
	if denier == nil || denier.RelayDenial() == nil {
		return context.WithCancel(ctx)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithTimeout(ctx, relayWithheldDialTimeout)
	}
	if time.Until(deadline) <= 2*relayAnswerMargin {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-relayAnswerMargin))
}
