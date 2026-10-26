package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/identitybootstrap"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func presentedCommandExitCode(failure commandFailure) int {
	if failure.execCount > 1 {
		return 1
	}
	if failure.execCount == 1 && failure.execCode > 0 && failure.execCode <= 255 {
		if failure.kind > classifyCommandFailure(failure.execCause).kind {
			return 1
		}
		return failure.execCode
	}
	if failure.kind == commandCanceled || failure.kind == commandInteractiveCanceled {
		return 130
	}
	return 1
}

func cliArgumentPresent(args []string, wanted ...string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if slices.Contains(wanted, argument) {
			return true
		}
	}
	return false
}

// commandFailure is a bounded classification of the actual owning error.
// Unary typed owners describe one outcome; independently joined failures must
// all be considered before treating cancellation or rejection as expected.
type commandFailureKind uint8

const (
	commandCanceled commandFailureKind = iota + 1
	commandInteractiveCanceled
	commandUsage
	commandRejected
	commandNativeExit
	commandOperational
	commandDeadline
	commandUnexpected
)

type commandFailure struct {
	supportReference string
	apiCauseInvalid  bool
	kind             commandFailureKind
	owner            error
	presented        bool
	execCount        int
	execCode         int
	execCause        error
}

func classifyCommandFailure(err error) (failure commandFailure) {
	defer func() {
		if recover() != nil {
			failure = commandFailure{kind: commandUnexpected, owner: err}
		}
	}()
	if err == nil {
		return commandFailure{}
	}
	type causeFrame struct {
		err    error
		status int
	}
	remaining := []causeFrame{{err: err}}
	referenceConflict := false
	seen := make(map[error]bool)
	result := commandFailure{}
	set := func(kind commandFailureKind, owner error) {
		if kind > result.kind {
			result.kind, result.owner = kind, owner
		}
	}
	for count := 0; len(remaining) > 0 && count < 32; count++ {
		frame := remaining[0]
		current := frame.err
		remaining = remaining[1:]
		if current == nil {
			set(commandUnexpected, err)
			continue
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
			if value.IsNil() {
				set(commandUnexpected, current)
				referenceConflict = true
				continue
			}
		}
		// A validated remote reference belongs to a captured terminal failure.
		// Only a leaf may borrow that ownership; joined siblings are still
		// visited and can require their own local failure report.
		if remote, ok := current.(interface{ SupportReference() string }); ok {
			reference := remote.SupportReference()
			_, unary := current.(interface{ Unwrap() error })
			_, joined := current.(interface{ Unwrap() []error })
			if supportref.Valid(reference) && !unary && !joined {
				if result.supportReference != "" && result.supportReference != reference {
					referenceConflict = true
				}
				result.supportReference = reference
				set(commandOperational, current)
				continue
			}
		}
		if uninstall, ok := current.(interface{ windowsUninstallFailureOwnership() (string, bool) }); ok {
			reference, remote := uninstall.windowsUninstallFailureOwnership()
			if supportref.Valid(reference) {
				if result.supportReference != "" && result.supportReference != reference {
					referenceConflict = true
				}
				result.supportReference = reference
			}
			if remote {
				set(commandOperational, current)
				continue
			}
		}
		if execFailure, ok := current.(execCommandFailure); ok {
			result.execCount++
			if result.execCount == 1 {
				result.execCode, result.execCause = execFailure.code, execFailure.cause
			}
			result.presented = result.presented || execFailure.presented
			remaining = append(remaining, causeFrame{err: execFailure.cause, status: frame.status})
			continue
		}
		if presented, ok := current.(presentedCommandFailure); ok {
			result.presented = true
			remaining = append(remaining, causeFrame{err: presented.cause, status: frame.status})
			continue
		}
		// HTTP metadata can accompany an API owner without creating a second
		// failure. It is authoritative only as a matching terminal leaf.
		if metadata, ok := current.(interface{ DiagnosticStatus() int }); ok && frame.status != 0 {
			_, unary := current.(interface{ Unwrap() error })
			_, joined := current.(interface{ Unwrap() []error })
			if !unary && !joined {
				if metadata.DiagnosticStatus() != frame.status {
					set(commandUnexpected, current)
				}
				continue
			}
		}
		if signedActivationPending(current) {
			set(commandRejected, commandRejection{reason: commandRejectActivationPending})
			continue
		}
		if current == hostinstall.ErrLegacyCarrierRunning {
			set(commandRejected, commandRejection{reason: commandRejectLegacyCarrierRunning})
			continue
		}
		if current == hostinstall.ErrUpdateVerificationFailed {
			set(commandRejected, commandRejection{reason: commandRejectUpdateVerification})
			continue
		}
		if current == hostinstall.ErrInsufficientSpace {
			set(commandRejected, commandRejection{reason: commandRejectInsufficientSpace})
			continue
		}
		if current == hostinstall.ErrUpdateNotReady {
			set(commandRejected, commandRejection{reason: commandRejectUpdateNotReady})
			continue
		}
		switch owner := current.(type) {
		case accessTunnelCauseError:
			switch owner.classification {
			case ErrAccessTunnelAuthentication, ErrAccessTunnelForbidden, ErrAccessTunnelRuntimeRPCUnavailable:
				set(commandRejected, current)
			case ErrAccessTunnelUnavailable:
				set(commandOperational, current)
			default:
				set(commandUnexpected, current)
			}
			if owner.cause != nil {
				status := frame.status
				if metadata, ok := owner.cause.(accessTunnelHTTPStatus); ok {
					status = metadata.status
				}
				remaining = append(remaining, causeFrame{err: owner.cause, status: status})
			}
			continue
		case commandRejection:
			if owner.valid() {
				set(commandRejected, current)
			} else {
				set(commandUnexpected, current)
			}
			if owner.cause != nil {
				remaining = append(remaining, causeFrame{err: owner.cause, status: frame.status})
			}
			continue
		case daemoncmd.ErrDaemonAccountMismatch, *daemoncmd.ApprovalRefusal, updateVersionUnsupportedError:
			set(commandRejected, current)
			continue
		case *inspectorDaemonError:
			// The daemon's stable inspector refusal carries its own message and next step.
			set(commandRejected, current)
			continue
		case usageError, unsupportedJSONOutputError, *preferences.InvalidError:
			set(commandUsage, current)
			continue
		case managedssh.NativeLaunchError:
			set(commandUnexpected, current)
			continue
		case managedssh.NativeExitError:
			set(commandNativeExit, current)
			continue
		case exitCodeError, jsonFailureExitCodeError:
			set(commandNativeExit, current)
			continue
		case *tunnel.RemoteExecError:
			switch owner.Code {
			case "exec_timeout":
				set(commandDeadline, current)
			case "exec_canceled", "canceled":
				set(commandCanceled, current)
			case "environment_unavailable", "exec_result_unavailable", "exec_failed", "exec_start_failed", "exec_start_uncertain", "exec_cancel_failed", "exec_wait_failed", "exec_already_running", "failed":
				set(commandOperational, current)
			default:
				set(commandUnexpected, current)
			}
			continue
		case *api.APIError:
			if supportref.Valid(owner.SupportReference) {
				if result.supportReference != "" && result.supportReference != owner.SupportReference {
					referenceConflict = true
				}
				result.supportReference = owner.SupportReference
			}
			if owner.Status == 0 || owner.Status >= 500 || owner.Status == 408 || owner.Status == 429 {
				set(commandOperational, current)
			} else {
				set(commandRejected, current)
			}
			if child := owner.Unwrap(); child != nil {
				result.apiCauseInvalid = result.apiCauseInvalid || !commandAPIStatusMetadataOnly(child, owner.Status)
				remaining = append(remaining, causeFrame{err: child, status: owner.Status})
			}
			continue
		case *envCommandFailure:
			if owner.cause == nil {
				set(commandUnexpected, current)
			} else {
				remaining = append(remaining, causeFrame{err: owner.cause, status: frame.status})
			}
			continue
		case *configComparisonFailure, *envHostRefreshFailure, updateCompletionFailure:
			set(commandOperational, current)
			if wrapper, ok := current.(interface{ Unwrap() error }); ok {
				if child := wrapper.Unwrap(); child != nil {
					remaining = append(remaining, causeFrame{err: child, status: frame.status})
				}
			}
			continue
		case *localapi.RemoteError:
			if owner.StatusCode >= 500 {
				set(commandOperational, current)
			} else {
				set(commandRejected, current)
			}
			continue
		case *TunnelCreateExistingError, *TunnelCreateChangedError, *TunnelOperationOutcomeError, *filetransfer.LimitError, updateOpenConnectionsRefusal, destinationStorageFullError, destinationBusyError, *sshTargetNotReadyError, *hostinstall.SuppliedExecutableError, *accessMachineStepError:
			set(commandRejected, current)
			continue
		case *TunnelOperationWaitTimeoutError:
			set(commandDeadline, current)
			continue
		}
		switch current {
		case hostdproto.ErrMaintenanceBusy:
			set(commandRejected, current)
			continue
		case context.DeadlineExceeded:
			set(commandDeadline, current)
			continue
		case context.Canceled:
			set(commandCanceled, current)
			continue
		case selector.ErrCanceled, selector.ErrInterrupted, prompt.ErrCanceled:
			set(commandInteractiveCanceled, current)
			continue
		case errUsage:
			set(commandUsage, current)
			continue
		case api.ErrUnauthenticated, config.ErrSecretNotFound, config.ErrNoCredentials, environmentmanager.ErrVaultPending, environmentmanager.ErrVaultNotSetUp, environmentmanager.ErrVaultLocked, environmentmanager.ErrVaultChanged, environmentmanager.ErrVaultTeamGrantRequired, environmentmanager.ErrVariableNotConfigured, identitybootstrap.ErrPairingRequired, identitybootstrap.ErrEnrollmentExpired, preferences.ErrChanged, preferences.ErrBusy, localwait.ErrMachineNotFound, localwait.ErrMachineAmbiguous, resolver.ErrMachineNotFound, resolver.ErrMachineAmbiguous, resolver.ErrSharedSessionAuthority, localwait.ErrInvalid, tailnet.ErrAuthority, tailnet.ErrAdmission:
			set(commandRejected, current)
			continue
		case tunnel.ErrTransportLost:
			set(commandOperational, current)
			continue
		case ErrAccessTunnelAuthentication, ErrAccessTunnelForbidden, ErrAccessTunnelRuntimeRPCUnavailable:
			set(commandRejected, current)
			continue
		case ErrAccessTunnelUnavailable:
			set(commandOperational, current)
			continue
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			set(commandOperational, current)
			continue
		}
		if _, ok := current.(syscall.Errno); ok {
			set(commandUnexpected, current)
			continue
		}
		if value.Type().Comparable() {
			if seen[current] {
				set(commandUnexpected, current)
				referenceConflict = true
				continue
			}
			seen[current] = true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > 32-len(remaining) {
				set(commandUnexpected, current)
				continue
			}
			for _, child := range children {
				remaining = append(remaining, causeFrame{err: child, status: frame.status})
			}
			continue
		}
		// Filesystem errors also implement net.Error. Keep their recovery local.
		if _, ok := current.(*os.PathError); ok {
			set(commandUnexpected, current)
			continue
		}
		// DNS failures may have no underlying error even though DNSError
		// implements Unwrap. The typed network failure is still actionable.
		if dns, ok := current.(*net.DNSError); ok && dns.Unwrap() == nil {
			set(commandOperational, current)
			continue
		}
		if _, ok := current.(net.Error); ok {
			// Network wrappers describe a transport only when their sole leaf
			// has no stronger typed meaning. Joins/cancellation/errno still
			// descend through the original chain rather than hiding siblings.
			if _, wrapped := current.(interface{ Unwrap() error }); !wrapped {
				set(commandOperational, current)
				continue
			}
			leaf := soleCommandOwner(current)
			if leaf != nil && classifyCommandFailure(leaf).kind == commandUnexpected {
				_, errno := leaf.(syscall.Errno)
				_, path := leaf.(*os.PathError)
				_, unary := leaf.(interface{ Unwrap() error })
				_, joined := leaf.(interface{ Unwrap() []error })
				if !errno && !path && !unary && !joined {
					set(commandOperational, current)
					continue
				}
			}
		}
		if unary, ok := current.(interface{ Unwrap() error }); ok {
			child := unary.Unwrap()
			if child == nil {
				set(commandUnexpected, current)
			} else {
				remaining = append(remaining, causeFrame{err: child, status: frame.status})
			}
			continue
		}
		set(commandUnexpected, current)
	}
	if len(remaining) > 0 || result.kind == 0 {
		set(commandUnexpected, err)
		referenceConflict = true
	}
	if referenceConflict {
		result.supportReference = ""
	}
	return result
}

func unexpectedCLIError(err error) bool { return classifyCommandFailure(err).kind == commandUnexpected }

func nativeExitFailure(err error) bool { return classifyCommandFailure(err).kind == commandNativeExit }

func operationalCLIError(err error) bool {
	kind := classifyCommandFailure(err).kind
	return kind == commandOperational || kind == commandDeadline
}

func userFacingError(err error) string {
	failure := classifyCommandFailure(err)
	if owner, ok := failure.owner.(commandRejection); ok && failure.kind == commandRejected && owner.valid() {
		return owner.Error()
	}
	return commandFailureMessage(err, failure)
}

// A typed result may state mutation facts only when no sibling error exists.
func soleCommandOwner(err error) error {
	for count := 0; err != nil && count < 32; count++ {
		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return nil
		}
		// The installer owns its fixed public prose even when several private
		// diagnostics caused it. An unrelated errors.Join still has no sole owner.
		if failure, ok := err.(*hostruntimecmd.CommandFailure); ok {
			return failure
		}
		if _, ok := err.(interface{ Unwrap() []error }); ok {
			return nil
		}
		if signedActivationPending(err) {
			return commandRejection{reason: commandRejectActivationPending}
		}
		if err == hostinstall.ErrLegacyCarrierRunning {
			return commandRejection{reason: commandRejectLegacyCarrierRunning}
		}
		if err == hostinstall.ErrUpdateVerificationFailed {
			return commandRejection{reason: commandRejectUpdateVerification}
		}
		if err == hostinstall.ErrInsufficientSpace {
			return commandRejection{reason: commandRejectInsufficientSpace}
		}
		if err == hostinstall.ErrUpdateNotReady {
			return commandRejection{reason: commandRejectUpdateNotReady}
		}
		switch err.(type) {
		case *sessionauth.CredentialFailure, *filetransfer.LimitError, updateOpenConnectionsRefusal, destinationStorageFullError, destinationBusyError, *sshTargetNotReadyError, *hostinstall.SuppliedExecutableError, *accessMachineStepError, commandRejection, *envCommandFailure, *envHostRefreshFailure, *configComparisonFailure, updateCompletionFailure, setupFailureError, commandPreparationFailure, *api.APIError, *localapi.RemoteError, *TunnelCreateExistingError, *TunnelCreateChangedError, *TunnelOperationOutcomeError, *TunnelOperationWaitTimeoutError, uninstallCleanupError, preferenceLoadFailure, usageError, unsupportedJSONOutputError, managedssh.NativeExitError, managedssh.NativeLaunchError, exitCodeError, jsonFailureExitCodeError:
			return err
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = wrapper.Unwrap()
	}
	return nil
}

func apiErrorFallback(err *api.APIError) string {
	switch err.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusConflict:
		return sentence(err.Error())
	case http.StatusForbidden:
		return "You do not have permission to perform this action. Check the selected account and target."
	case http.StatusNotFound:
		return "The requested Paperboat resource was not found. Refresh the available targets and retry."
	case http.StatusTooManyRequests:
		return "Paperboat is receiving too many requests. Wait a moment, then retry."
	}
	if err.Status >= 500 || err.Status == 0 {
		return "Paperboat is temporarily unavailable. Retry in a moment; if this continues, run `pb doctor`."
	}
	return "Paperboat could not complete the request. Retry the command; if this continues, run `pb doctor`."
}

func sentence(message string) string {
	message = strings.TrimSpace(message)
	if message == "" || strings.ContainsAny(message[len(message)-1:], ".!?") {
		return message
	}
	return message + "."
}

func isCobraUsageError(err error) bool { return classifyCommandFailure(err).kind == commandUsage }
