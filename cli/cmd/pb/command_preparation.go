package main

import (
	"context"
	"fmt"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type preparationStep uint8

const (
	prepareMachineRegistration preparationStep = iota + 1
	prepareInboxRegistration
	prepareMachineUser
	prepareMachineName
	prepareInboxPreservation
	prepareSelectedFile
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
	case prepareInboxRegistration:
		return "Paperboat could not read the Inbox registration. Run `pb doctor`, then retry the Inbox command."
	case prepareMachineUser:
		return "Paperboat could not resolve the local operating-system user. Check the user account, then retry `pb setup`."
	case prepareMachineName:
		return "Paperboat could not resolve this machine's hostname. Check the operating-system hostname; for setup, pass --name."
	case prepareInboxPreservation:
		return "Paperboat could not verify Inbox preservation. Check the machine registration with `pb doctor`, then retry `pb uninstall`."
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

func reportDeliveredReceiptFailure(ctx context.Context, writer io.Writer, jsonOutput bool, err error) {
	reference := supportref.FromContext(ctx)
	if !errorreport.HTTPAttemptObserved(err) {
		reference = errorreport.Current().ObserveFailure(ctx, "pb", "send", "control_request", "control_request_failed", err).SupportReference
	}
	if !jsonOutput {
		fmt.Fprintf(writer, "Warning: delivery completed, but receipt status could not be updated. Inspect `pb send status` before retrying delivery. Support reference: %s.\n", reference)
	}
}
