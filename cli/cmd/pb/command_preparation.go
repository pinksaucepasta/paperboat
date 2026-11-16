package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

type preparationStep uint8

const (
	prepareMachineRegistration preparationStep = iota + 1
	prepareInboxRegistration
	prepareMachineUser
	prepareMachineName
	prepareInboxPreservation
	prepareSelectedFile
	prepareOpenSSH
	prepareMachineNotAdded
	prepareDoctorRepair
	prepareConfigService
	prepareSSHConfiguration
	prepareSSHIdentity
	prepareSSHAgent
	prepareSSHTransport
	prepareSSHCertificate
	prepareSSHHostVerification
)

// Only finite local phases become public prose. The original error remains
// available to the invocation's diagnostic owner without formatting its data.
type commandPreparationFailure struct {
	step  preparationStep
	cause error
}

func (e commandPreparationFailure) Unwrap() error { return e.cause }
func (e commandPreparationFailure) Error() string {
	switch e.step {
	case prepareMachineRegistration:
		return "Paperboat could not read this machine's registration. Run `pb doctor`, then repair the state or run `pb setup`."
	case prepareMachineNotAdded:
		return "This computer has not been added to Paperboat yet. Run `pb login` (or `pb setup`) to add it, then retry."
	case prepareInboxRegistration:
		return "Paperboat could not read the Inbox registration. Run `pb doctor`, then retry the Inbox command."
	case prepareMachineUser:
		return "Paperboat could not resolve the local operating-system user. Check the user account, then retry `pb setup`."
	case prepareMachineName:
		return "Paperboat could not resolve this machine's hostname. Check the operating-system hostname; for setup, pass --name."
	case prepareInboxPreservation:
		return "Paperboat could not verify Inbox preservation. Check the machine registration with `pb doctor`, then retry `pb uninstall`."
	case prepareOpenSSH:
		return "A supported OpenSSH client is unavailable. Install OpenSSH or repair its PATH entry, then retry `pb ssh doctor`."
	case prepareSSHConfiguration:
		return "Paperboat's managed OpenSSH configuration is not ready. Run `pb login` again to rewrite it, then retry `pb ssh doctor`."
	case prepareSSHIdentity:
		return "Paperboat's managed SSH key is not ready on this computer. Run `pb login` again, then retry `pb ssh doctor`."
	case prepareSSHAgent:
		return "Paperboat's SSH agent on this computer does not answer. Run `pb doctor` (the daemon may be stopped or restarting) or `pb login` again, then retry `pb ssh doctor`."
	case prepareSSHTransport:
		return "Paperboat could not open an SSH stream to the machine. Check that it is online with `pb machine list`, then retry `pb ssh doctor`."
	case prepareSSHCertificate:
		return "Paperboat could not get an SSH certificate for the machine. Check `pb doctor` and your access to the machine, then retry `pb ssh doctor`."
	case prepareSSHHostVerification:
		return "The machine's SSH login check failed: its host key or the managed key setup is not ready. Wait a minute for a new or reinstalled machine, then retry `pb ssh doctor`."
	case prepareDoctorRepair:
		if errors.Is(e.cause, elevation.ErrElevationRestrictedJob) {
			return "Paperboat could not repair the installation: this terminal runs inside a restricted job (common in IDEs and CI) that can't start the administrator helper. Open a normal PowerShell window and run `pb doctor --repair` again."
		}
		if errors.Is(e.cause, elevation.ErrElevationDenied) {
			return "Paperboat could not repair the installation: administrator approval was not given. Run `pb doctor --repair` again and approve the Windows prompt."
		}
		return "Paperboat could not repair the installation: the repair did not finish. Run `pb doctor` to see the current state, then retry `pb doctor --repair`."
	case prepareConfigService:
		if errors.Is(e.cause, elevation.ErrElevationRestrictedJob) {
			return "Paperboat could not start or stop the config sync service: this terminal runs inside a restricted job (common in IDEs and CI) that can't start the administrator helper. Open a normal PowerShell window and retry."
		}
		if errors.Is(e.cause, elevation.ErrElevationDenied) {
			return "Paperboat could not start or stop the config sync service: administrator approval was not given. Retry and approve the Windows prompt."
		}
		return "Paperboat could not start or stop the config sync service on this machine. Run `pb doctor` to see the current state, then retry."
	case prepareSelectedFile:
		return "A selected file or folder is unavailable. Check that it exists and is readable, then select it again."
	default:
		return "Paperboat could not prepare the command. Run `pb doctor`, then retry."
	}
}

type setupFailureError struct{ cause error }

func (e setupFailureError) Unwrap() error { return e.cause }
func (setupFailureError) Error() string {
	return "Machine setup did not finish. Some setup steps may have completed. Run `pb doctor`, then retry `pb setup` to resume."
}

type deliveredReceiptFailure struct{ causes []error }

func (deliveredReceiptFailure) Error() string {
	return "delivery completed, but receipt status could not be updated"
}

func (e deliveredReceiptFailure) Unwrap() []error { return e.causes }

func reportDeliveredReceiptFailure(ctx context.Context, writer io.Writer, jsonOutput bool, err error) {
	if err == nil {
		return
	}
	attemptObserved := errorreport.HTTPAttemptObserved(err)
	if jsonOutput {
		if !attemptObserved {
			errorreport.Current().ObserveFailure(ctx, "pb", "send", "control_request", "control_request_failed", err)
		}
		return
	}
	reference := supportref.FromContext(ctx)
	if !attemptObserved && reference == "" {
		reference = errorreport.ProjectFault(ctx, "pb", "send", "control_request", "control_request_failed", err).SupportReference
		ctx = supportref.WithContext(ctx, reference)
	}
	_, warningErr := fmt.Fprintf(writer, "Warning: delivery completed, but receipt status could not be updated. Inspect `pb send status` before retrying delivery. Support reference: %s.\n", reference)
	if attemptObserved && warningErr == nil {
		return
	}
	observationErr := err
	if warningErr != nil {
		causes := []error{err, warningErr}
		if attemptObserved {
			causes = []error{warningErr}
		}
		observationErr = deliveredReceiptFailure{causes: causes}
	}
	errorreport.Current().ObserveFailure(ctx, "pb", "send", "control_request", "control_request_failed", observationErr)
}
