package native

import (
	"errors"

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
	if err == nil || denier == nil || !errors.As(err, &failure) || failure.stage != "peer_connect" {
		return err
	}
	if denial := denier.RelayDenial(); denial != nil {
		return &RelayQuotaError{Denial: denial, Err: err}
	}
	return err
}
