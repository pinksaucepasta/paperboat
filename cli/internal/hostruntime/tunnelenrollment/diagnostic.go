package tunnelenrollment

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connectorrotation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

// ActivationDiagnosticCode is a small, local-safe classification for an
// activation failure. It deliberately contains no provider message, URL, or
// credential material.
type ActivationDiagnosticCode string

const (
	ActivationDiagnosticLifecycleUnavailable   ActivationDiagnosticCode = "lifecycle_unavailable"
	ActivationDiagnosticControlHTTPDenied      ActivationDiagnosticCode = "control_http_denied"
	ActivationDiagnosticControlHTTPUnavailable ActivationDiagnosticCode = "control_http_unavailable"
	ActivationDiagnosticControlNetworkTLS      ActivationDiagnosticCode = "control_network_tls"
	ActivationDiagnosticInvalidSessionConfig   ActivationDiagnosticCode = "invalid_session_config"
	ActivationDiagnosticAssemblyResolution     ActivationDiagnosticCode = "assembly_resolution"
	ActivationDiagnosticAssemblyOpen           ActivationDiagnosticCode = "assembly_open"
	ActivationDiagnosticAssemblyBinding        ActivationDiagnosticCode = "assembly_binding"
	ActivationDiagnosticAssemblyStart          ActivationDiagnosticCode = "assembly_start"
)

// ActivationDiagnostic retains the typed cause for local callers while its
// Error method remains safe to expose through the local enrollment endpoint.
// The cause is never serialized by the HTTP adapter.
type ActivationDiagnostic struct {
	Code  ActivationDiagnosticCode
	Cause error
}

func (e *ActivationDiagnostic) Error() string {
	if e == nil || !validActivationDiagnosticCode(e.Code) {
		return "activation diagnostic unavailable"
	}
	return "activation diagnostic: " + string(e.Code)
}

func (e *ActivationDiagnostic) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *ActivationDiagnostic) Is(target error) bool {
	return target == ErrActivation
}

func validActivationDiagnosticCode(code ActivationDiagnosticCode) bool {
	switch code {
	case ActivationDiagnosticLifecycleUnavailable,
		ActivationDiagnosticControlHTTPDenied,
		ActivationDiagnosticControlHTTPUnavailable,
		ActivationDiagnosticControlNetworkTLS,
		ActivationDiagnosticInvalidSessionConfig,
		ActivationDiagnosticAssemblyResolution,
		ActivationDiagnosticAssemblyOpen,
		ActivationDiagnosticAssemblyBinding,
		ActivationDiagnosticAssemblyStart:
		return true
	default:
		return false
	}
}

func activationDiagnosticCodeOf(err error) (code string) {
	defer func() {
		if recover() != nil {
			code = ""
		}
	}()
	// Only a sole, cause-free domain outcome can describe invalid local
	// configuration. Provider/cleanup siblings retain their operational meaning.
	for depth := 0; depth < 32 && err != nil; depth++ {
		if diagnostic, ok := err.(*ActivationDiagnostic); ok {
			if diagnostic != nil && validActivationDiagnosticCode(diagnostic.Code) {
				return string(diagnostic.Code)
			}
			return ""
		}
		if protocol, ok := err.(*connectorprotocol.Error); ok {
			if protocol == nil || protocol.Cause != nil || protocol.Code == connectorprotocol.CodeCredentialRotationFailed {
				return ""
			}
			break
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) != 1 {
				return ""
			}
			err = children[0]
		} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			err = wrapped.Unwrap()
		} else {
			break
		}
		if depth == 31 {
			return ""
		}
	}
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, tunnelmanager.ErrProductionIdentityMissing),
		errors.Is(err, tunnelmanager.ErrProductionControlMissing):
		return string(ActivationDiagnosticLifecycleUnavailable)
	case connectorprotocol.CodeOf(err) != "",
		errors.Is(err, connectorrotation.ErrControlSessionInvalid),
		errors.Is(err, tunnelmanager.ErrProductionAssemblyInvalid),
		errors.Is(err, tunnelmanager.ErrProductionControlRestartRequired):
		return string(ActivationDiagnosticInvalidSessionConfig)
	default:
		return ""
	}
}

// ActivationDiagnosticCodeOf returns only a finite, local-safe diagnostic
// code. It is intended for production observation; callers must not log the
// original error alongside it.
func ActivationDiagnosticCodeOf(err error) string { return activationDiagnosticCodeOf(err) }
