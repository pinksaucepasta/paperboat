package main

import (
	"reflect"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/spf13/cobra"
)

func homeFailureMessage(command *cobra.Command, err error) string {
	message := userFacingError(err)
	kind := classifyCommandFailure(err).kind
	if kind == commandUnexpected || kind == commandOperational || kind == commandDeadline {
		if reference := supportref.FromContext(command.Context()); supportref.Valid(reference) && !strings.Contains(message, "Support reference:") {
			message += " Support reference: " + reference + "."
		}
	}
	return message
}

// Only a single displayed result owns suppression. An independent joined
// failure still needs presentation and evidence; malformed chains fail closed.
func homeFailureAlreadyDisplayed(err error) bool {
	for depth := 0; err != nil && depth < 32; depth++ {
		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		if displayed, ok := err.(*homeResultError); ok {
			return displayed.Err != nil
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapper.Unwrap()
	}
	return false
}

// Menu actions remain available after a failure, making this the final owner
// for the consumed error rather than the process command boundary.
func observeHomeFailure(command *cobra.Command, err error) {
	failure := classifyCommandFailure(err)
	if err == nil || errorreport.HTTPAttemptObserved(err) {
		return
	}
	ctx := command.Context()
	if remote, ok := failure.owner.(*api.APIError); ok && supportref.Valid(remote.SupportReference) {
		ctx = supportref.WithContext(ctx, remote.SupportReference)
	}
	operation := command
	for operation.Parent() != nil && operation.Parent().Parent() != nil {
		operation = operation.Parent()
	}
	name := errorreport.Operation(operation.Name())
	switch failure.kind {
	case commandUnexpected:
		errorreport.Current().CaptureFailure(ctx, "pb", name, "command", "unexpected_cli_failure", err)
	case commandOperational, commandDeadline, commandRejected:
		errorreport.Current().ObserveFailure(ctx, "pb", name, "command", "command_failed", err)
	}
}
