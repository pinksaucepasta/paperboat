package updated

import (
	"errors"
	"strings"
)

var ErrCustomInstallation = errors.New("Custom builds cannot use official updates. Install a fresh official build to switch to official updates.")

const maxControlErrorMessage = 2048

// ControlError is a bounded diagnostic returned by the local managed updater.
// Code remains stable for automation; Message carries local recovery evidence
// so pb update does not collapse every signing, staging, or SCM failure into a
// generic check_failed string.
type ControlError struct {
	Code      string
	Message   string
	operation string
}

func (e *ControlError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// DiagnosticStage identifies the bounded local updater operation that
// returned the control error. The operation comes from the fixed request
// protocol, never from the server's free-form message.
func (e *ControlError) DiagnosticStage() string {
	if e == nil {
		return "control_request"
	}
	switch e.operation {
	case "check":
		return "update_check"
	case "download":
		return "update_download"
	case "install":
		return "update_install"
	default:
		return "control_request"
	}
}

// DiagnosticCode retains only known protocol error codes. Unknown server
// values and the bounded but free-form message stay out of local diagnostics.
func (e *ControlError) DiagnosticCode() string {
	if e == nil {
		return "control_request_failed"
	}
	switch e.Code {
	case "invalid_request":
		return "updater_invalid_request"
	case "custom_installation":
		return "updater_custom_installation"
	case "approval_required":
		return "updater_approval_required"
	case "candidate_changed":
		return "updater_candidate_changed"
	case "active_terminal_sessions":
		return "updater_active_terminal_sessions"
	case "activation_unavailable":
		return "updater_activation_unavailable"
	case "recovery_required":
		return "updater_recovery_required"
	case "release_not_found":
		return "updater_release_not_found"
	case "update_failed":
		return "updater_update_failed"
	case "check_failed":
		return "updater_check_failed"
	default:
		return "control_request_failed"
	}
}

func boundedControlErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Map(func(character rune) rune {
		if character == '\x00' || character == '\r' || character == '\n' || character < 0x20 && character != '\t' {
			return ' '
		}
		return character
	}, err.Error())
	if len(message) > maxControlErrorMessage {
		message = message[:maxControlErrorMessage]
	}
	return message
}

func validControlErrorMessage(message string) bool {
	if len(message) > maxControlErrorMessage {
		return false
	}
	for _, character := range message {
		if character == '\x00' || character == '\r' || character == '\n' || character < 0x20 && character != '\t' {
			return false
		}
	}
	return true
}

func validControlResponseError(status, code, message string) bool {
	if !validControlErrorMessage(message) {
		return false
	}
	if status == "error" {
		return code != ""
	}
	return code == "" && message == ""
}
