package inspectorauth

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
)

const (
	controlRequestStage = "control_request"
	controlRequestCode  = "control_request_failed"
	peerAuthorityStage  = "peer_authority"
	peerAuthorityCode   = "peer_authority_failed"
)

var errInvalidDecision = errors.New("inspector authority returned an invalid decision")

type diagnosticFailure struct {
	classification error
	stage          string
	code           string
	cause          error
}

func (e *diagnosticFailure) Error() string {
	if e.stage == peerAuthorityStage {
		return "inspector authority decision could not be verified"
	}
	return "inspector authorization request failed"
}

func (e *diagnosticFailure) Unwrap() error           { return e.cause }
func (e *diagnosticFailure) Is(target error) bool    { return target == e.classification }
func (e *diagnosticFailure) DiagnosticStage() string { return e.stage }
func (e *diagnosticFailure) DiagnosticCode() string  { return e.code }

func controlRequestFailure(classification, cause error) error {
	return &diagnosticFailure{classification: classification, stage: controlRequestStage, code: controlRequestCode, cause: cause}
}

func invalidDecisionFailure() error {
	return &diagnosticFailure{classification: inspectorapi.ErrUpstream, stage: peerAuthorityStage, code: peerAuthorityCode, cause: errInvalidDecision}
}
