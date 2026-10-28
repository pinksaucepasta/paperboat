package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
)

type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return "" }

func (e exitCodeError) ExitCode() int { return e.code }

// The command already wrote its structured result; retain the failure for the
// invocation owner without writing a second JSON document or losing its cause.
type presentedCommandFailure struct{ cause error }

func (e presentedCommandFailure) Error() string { return "command did not finish" }

func (e presentedCommandFailure) Unwrap() error { return e.cause }

func (e presentedCommandFailure) ExitCode() int {
	if classifyCommandFailure(e.cause).kind == commandCanceled {
		return 130
	}
	return 1
}

// execCommandFailure retains the actual cause after the exec-event owner has
// presented its result. The reserved code describes the command wire outcome.
type execCommandFailure struct {
	cause     error
	code      int
	presented bool
}

func (execCommandFailure) Error() string {
	return "Remote execution did not finish. Check the machine connection and retry; the remote outcome may be unknown."
}

func (e execCommandFailure) Unwrap() error { return e.cause }

func (e execCommandFailure) ExitCode() int { return e.code }

type preferenceLoadFailure struct{ cause error }

func (preferenceLoadFailure) Error() string {
	return "Local command preferences could not be loaded. Retry with --no-customization or run `pb config customize`."
}

func (e preferenceLoadFailure) Unwrap() error { return e.cause }

type usageError struct {
	err           error
	publicMessage string
	// refusal marks a state the user fixes, not wrong arguments, so the command help is not printed.
	refusal bool
}

func (e usageError) Error() string {
	if e.publicMessage != "" {
		return e.publicMessage
	}
	return "The command arguments are invalid. Run `pb COMMAND --help` and retry."
}

func (e usageError) Unwrap() error { return e.err }

func (e usageError) Is(target error) bool { return target == errUsage }

// printsUsage reports whether a usage failure is wrong arguments, so the command help helps.
func printsUsage(err error) bool {
	var misuse usageError
	return !errors.As(err, &misuse) || !misuse.refusal
}

type uninstallCleanupError struct{ err error }

func (e uninstallCleanupError) Error() string {
	steps := uninstallFailureStepNames(e.err)
	if len(steps) == 0 {
		return "Paperboat local removal was incomplete. Retry `pb uninstall`."
	}
	return "Paperboat local removal was incomplete after attempting every step. Failed: " + strings.Join(steps, ", ") + ". Retry `pb uninstall`."
}

func (e uninstallCleanupError) Unwrap() error { return e.err }

type uninstallStepFailure struct {
	name string
	err  error
}

func (e uninstallStepFailure) Error() string { return e.name + ": Paperboat cleanup step failed" }

func (e uninstallStepFailure) Unwrap() error { return e.err }

func uninstallFailureStepNames(err error) []string {
	allowed := map[string]bool{"remove installed CLI manuals": true, "resolve Paperboat Inbox preservation": true, "stop local daemon": true, "remove managed OpenSSH configuration": true, "remove managed SSH public identity": true, "remove system Paperboat runtime": true, "remove installed Paperboat product": true, "remove user Paperboat state": true}
	seen := make(map[string]bool)
	var names []string
	remaining := []error{err}
	for count := 0; len(remaining) > 0 && count < 32; count++ {
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			continue
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			continue
		}
		if failure, ok := current.(uninstallStepFailure); ok {
			if allowed[failure.name] && !seen[failure.name] {
				seen[failure.name] = true
				names = append(names, failure.name)
			}
			continue
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) <= 32-len(remaining) {
				remaining = append(remaining, children...)
			}
		} else if unary, ok := current.(interface{ Unwrap() error }); ok {
			remaining = append(remaining, unary.Unwrap())
		}
	}
	return names
}

func invocationError(err error) error {
	if err == nil {
		return nil
	}
	return usageError{err: err}
}

// localArgumentError exposes only command-owned constraint text, never user values
// or errors returned by a provider. Other invocation errors remain redacted.
func localArgumentError(message string) error {
	return usageError{err: errors.New(message), publicMessage: message}
}

func commandArgs(args cobra.PositionalArgs) cobra.PositionalArgs {
	return func(command *cobra.Command, values []string) error {
		return invocationError(args(command, values))
	}
}

func terminalArgs(minimum int) cobra.PositionalArgs {
	return func(_ *cobra.Command, values []string) error {
		if len(values) < minimum || len(values) > 2 {
			return fmt.Errorf("accepts between %d and 2 arg(s), received %d", minimum, len(values))
		}
		if len(values) == 2 && values[1] != "new" {
			return fmt.Errorf("second argument must be `new`")
		}
		return nil
	}
}
