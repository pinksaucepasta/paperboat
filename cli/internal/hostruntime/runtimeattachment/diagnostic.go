package runtimeattachment

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
)

const (
	controlRequestStage = "control_request"
	controlRequestCode  = "control_request_failed"
	peerConnectStage    = "peer_connect"
	transportFailure    = "transport_failed"
	peerAuthorityStage  = "peer_authority"
	peerAuthorityCode   = "peer_authority_failed"
	streamOpenStage     = "stream_open"
)

type diagnosticFailure struct {
	stage, code string
	cause       error
}

func (e *diagnosticFailure) Error() string {
	switch e.stage {
	case controlRequestStage:
		return "runtime attachment request failed"
	case peerAuthorityStage:
		return "runtime carrier authority was rejected"
	case peerConnectStage:
		return "runtime carrier connection failed"
	case streamOpenStage:
		return "runtime carrier stream setup failed"
	default:
		return "runtime attachment reconciliation failed"
	}
}

func (e *diagnosticFailure) Unwrap() error           { return e.cause }
func (e *diagnosticFailure) DiagnosticStage() string { return e.stage }
func (e *diagnosticFailure) DiagnosticCode() string  { return e.code }

func classifyFailure(err error, stage, code string) error {
	if err == nil {
		return nil
	}
	return &diagnosticFailure{stage: stage, code: code, cause: err}
}

func controlFailure(err error) error {
	return classifyFailure(err, controlRequestStage, controlRequestCode)
}

func peerConnectFailure(err error) error {
	return classifyFailure(err, peerConnectStage, transportFailure)
}

func peerAuthorityFailure(err error) error {
	return classifyFailure(err, peerAuthorityStage, peerAuthorityCode)
}

func streamOpenFailure(err error) error {
	return classifyFailure(err, streamOpenStage, transportFailure)
}

func cleanupFailure(err error) error {
	return classifyFailure(err, "lifecycle", "service_failed")
}

func carrierFailure(err error) error {
	if errors.Is(err, preview.ErrMachineAttachmentTrustRequired) || errors.Is(err, preview.ErrMachineAttachmentSessionInvalid) {
		return peerAuthorityFailure(err)
	}
	return peerConnectFailure(err)
}
