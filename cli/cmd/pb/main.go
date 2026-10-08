// Command pb is the invisible terminal wrapper for the
// Paperboat platform. `pb <environment>` attaches an enrolled machine
// through Paperboat auth and bridges local file pastes into
// remote TUIs. Cross-service calls run behind interfaces so protocol behavior
// remains independently testable.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	bugreportpkg "github.com/pinksaucepasta/paperboat/internal/bugreport"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemoncmd"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/fileindex"
	filetransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	service "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/machinename"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/paste"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/identitybootstrap"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/recoverykey"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/processlifetime"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/remotepath"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/selfupdate"
	"github.com/pinksaucepasta/paperboat/internal/session"
	"github.com/pinksaucepasta/paperboat/internal/statusbar"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/internal/userpaths"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func main() {
	os.Exit(mainExit())
}

func mainExit() (exitCode int) {
	ctx := supportref.WithContext(context.Background(), supportref.New())
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	reporter := errorreport.FromEnvironment()
	restoreReporter := errorreport.Install(reporter)
	defer restoreReporter()
	component := processComponent(os.Args[1:])
	ctx, closeDiagnostics := openProcessDiagnostics(ctx, os.Args[1:], reporter, os.Stderr)
	defer closeDiagnostics()
	defer func() {
		if failure := recover(); failure != nil {
			panicErr, ok := failure.(error)
			if !ok {
				panicErr = errors.New("process panic")
			}
			reporter.CaptureFailure(ctx, component, "process", "process", "process_panic", panicErr)
			if jsonArgumentRequested(os.Args[1:]) {
				_ = writeCLIJSONError(os.Stdout, &api.APIError{Code: "unexpected_failure", SupportReference: supportref.FromContext(ctx)})
			} else {
				fmt.Fprintf(os.Stderr, "pb: Paperboat stopped unexpectedly. Retry; if this continues, contact support with reference %s.\n", supportref.FromContext(ctx))
			}
			exitCode = 1
		}
	}()
	return runWithReporter(ctx, os.Args[1:], os.Stdout, os.Stderr, reporter)
}

func processComponent(args []string) string {
	root := newRootCommand()
	command, _, err := root.Find(args)
	if err != nil || command == nil {
		return "pb"
	}
	for command.Parent() != nil && command.Parent() != root {
		command = command.Parent()
	}
	if command.Name() == "daemon" || strings.HasPrefix(command.Name(), "__runtime-") {
		return "paperboatd"
	}
	return "pb"
}

var errUsage = errors.New("command usage error")

// currentLocalDaemonPaths is kept behind a small seam so command tests can
// use a private local API endpoint. Production callers use the platform
// owner's stable paths supplied by localdaemon.CurrentUserPaths.
var currentLocalDaemonPaths = localdaemon.CurrentUserPaths

// installLocalDaemonService owns the one production boundary that registers
// the local daemon. Command tests replace it with an in-process sentinel: a
// Go test executable must never be registered as a detached daemon service.
var installLocalDaemonService localDaemonServiceInstaller = localdaemon.InstallCurrentUserService

// removeLocalDaemonService is the matching ownership boundary used when setup
// changes the machine identity that the daemon must present. Rebinding stops
// the exact owner process before installing the replacement service.
var removeLocalDaemonService localDaemonServiceRemover = localdaemon.RemoveCurrentUserService

var verifySetupInstallSource = hostruntimecmd.VerifyInstallSource
var setupBackendClient = backendClient

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
}

func (e usageError) Error() string {
	if e.publicMessage != "" {
		return e.publicMessage
	}
	return "The command arguments are invalid. Run `pb COMMAND --help` and retry."
}
func (e usageError) Unwrap() error        { return e.err }
func (e usageError) Is(target error) bool { return target == errUsage }

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

func (e uninstallStepFailure) Error() string { return "Paperboat cleanup step failed" }
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

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWithReporter(ctx, args, stdout, stderr, nil)
}

func runWithReporter(ctx context.Context, args []string, stdout, stderr io.Writer, reporter *errorreport.Reporter) (exitCode int) {
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	root := newRootCommand()
	operation := "command"
	if resolved, _, err := root.Find(args); err == nil && resolved != nil {
		for resolved.Parent() != nil && resolved.Parent() != root {
			resolved = resolved.Parent()
		}
		operation = errorreport.Operation(resolved.Name())
	}
	ctx, finishObservation := reporter.Start(ctx, processComponent(args), operation)
	outcome := "failed"
	defer func() {
		if failure := recover(); failure != nil {
			finishObservation("failed")
			panic(failure)
		}
		if exitCode != 0 && outcome == "success" {
			outcome = "failed"
		}
		if exitCode == 0 && outcome == "failed" {
			outcome = "success"
		}
		finishObservation(outcome)
	}()
	root.SetOut(stdout)
	root.SetErr(stderr)
	if jsonArgumentRequested(args) {
		if cliArgumentPresent(args, "--version", "-v") {
			outcome = "success"
			if err := writeCLIJSON(stdout, map[string]any{"command": "pb", "version": buildinfo.Version}); err != nil {
				return 1
			}
			return 0
		}
		if cliArgumentPresent(args, "--help", "-h") {
			outcome = "success"
			if err := writeCLIJSON(stdout, cliCommandHelp(root, args)); err != nil {
				return 1
			}
			return 0
		}
	}
	resolvedArgs, preferenceContext, preferenceErr := preparePreferences(root, args, ctx)
	var executed *cobra.Command
	err := preferenceErr
	if err != nil {
		err = preferenceLoadFailure{cause: err}
	}
	if err == nil {
		if _, _, findErr := root.Find(resolvedArgs); findErr != nil {
			err = invocationError(findErr)
		} else {
			root.SetArgs(resolvedArgs)
			executed, err = root.ExecuteContextC(preferenceContext)
		}
	}
	if err == nil {
		outcome = "success"
		return 0
	}
	failure := classifyCommandFailure(err)
	switch failure.kind {
	case commandCanceled, commandInteractiveCanceled:
		outcome = "canceled"
	case commandUsage, commandRejected:
		outcome = "rejected"
	}
	diagnosticReference := ""
	if failure.kind != commandCanceled && failure.kind != commandInteractiveCanceled {
		faultCtx := ctx
		if supportref.Valid(failure.supportReference) {
			faultCtx = supportref.WithContext(ctx, failure.supportReference)
		}
		if failure.kind == commandUnexpected {
			diagnosticReference = reporter.CaptureFailure(faultCtx, processComponent(args), operation, "command", "unexpected_cli_failure", err).SupportReference
		} else if failure.kind == commandOperational || failure.kind == commandDeadline || failure.kind == commandRejected {
			diagnosticReference = reporter.ObserveFailure(faultCtx, processComponent(args), operation, "command", "command_failed", err).SupportReference
		}
	}

	jsonOutput := jsonArgumentRequested(args)
	if executed != nil && jsonOutputRequested(executed) {
		jsonOutput = true
	}
	if failure.presented {
		return presentedCommandExitCode(failure)
	}
	if failure.kind == commandInteractiveCanceled {
		if jsonOutput {
			_ = writeCLIJSONError(stdout, err)
			return 130
		}
		return 0
	}
	if failure.kind == commandCanceled {
		for command := executed; command != nil; command = command.Parent() {
			if command.Name() == "daemon" && command.Parent() == root {
				return 0
			}
		}
		if jsonOutput {
			_ = writeCLIJSONError(stdout, err)
		} else {
			fmt.Fprintln(stderr, "pb: Operation canceled.")
		}
		return 130
	}
	if failure.kind == commandUsage {
		if jsonOutput {
			_ = writeCLIJSONError(stdout, err)
		} else {
			fmt.Fprintln(stderr, "pb:", commandFailureMessage(err, failure))
			root.SetOut(stderr)
			_ = root.Usage()
		}
		return 2
	}
	if failure.kind == commandNativeExit {
		if exit, ok := failure.owner.(interface{ ExitCode() int }); ok {
			if _, ok := failure.owner.(jsonFailureExitCodeError); ok && jsonOutput {
				_ = writeCLIJSONError(stdout, err)
			}
			if code := exit.ExitCode(); code >= 0 && code <= 255 {
				return code
			}
		}
		return 1
	}

	if jsonOutput {
		_ = writeCLIJSONErrorWithReference(stdout, err, diagnosticReference)
		return 1
	}
	if message := commandFailureMessage(err, failure); message != "" {
		if diagnosticReference != "" && !strings.Contains(message, diagnosticReference) {
			message = sentence(message) + " Support reference: " + diagnosticReference + "."
		}
		fmt.Fprintln(stderr, "pb:", message)
	}
	return 1
}

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
		switch owner := current.(type) {
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
			case "exec_result_unavailable", "exec_failed", "exec_start_failed", "exec_start_uncertain", "exec_cancel_failed", "exec_wait_failed", "exec_already_running", "failed":
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
		case *configComparisonFailure, *envHostRefreshFailure:
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
		case *TunnelCreateExistingError, *TunnelCreateChangedError, *TunnelOperationOutcomeError:
			set(commandRejected, current)
			continue
		case *TunnelOperationWaitTimeoutError:
			set(commandDeadline, current)
			continue
		}
		switch current {
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
		case api.ErrUnauthenticated, config.ErrSecretNotFound, config.ErrNoCredentials, environmentmanager.ErrVaultPending, environmentmanager.ErrVaultLocked, environmentmanager.ErrVaultChanged, environmentmanager.ErrVaultTeamGrantRequired, environmentmanager.ErrVariableNotConfigured, identitybootstrap.ErrPairingRequired, identitybootstrap.ErrEnrollmentExpired, preferences.ErrChanged, preferences.ErrBusy, localwait.ErrMachineNotFound, localwait.ErrMachineAmbiguous, localwait.ErrInvalid, tailnet.ErrAuthority, tailnet.ErrAdmission:
			set(commandRejected, current)
			continue
		case tunnel.ErrTransportLost:
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
func nativeExitFailure(err error) bool  { return classifyCommandFailure(err).kind == commandNativeExit }
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

func commandFailureMessage(err error, failure commandFailure) string {
	if err == nil {
		return ""
	}
	if preference, ok := soleCommandOwner(err).(preferenceLoadFailure); ok {
		return preference.Error()
	}
	const generic = "Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues."
	if failure.kind == commandRejected {
		if _, accountCredentials := soleCommandOwner(err).(*sessionauth.CredentialFailure); accountCredentials && onlyEnvironmentFailureLeaves(err, func(leaf error) bool {
			return leaf == config.ErrSecretNotFound || leaf == config.ErrNoCredentials
		}) {
			return "Paperboat sign-in credentials are unavailable. " + dashboardEnrollmentGuidance + " Then retry."
		}
		if rejected, ok := soleCommandOwner(err).(commandRejection); ok && rejected.valid() {
			return rejected.Error()
		}
	}
	switch owned := soleCommandOwner(err).(type) {
	case *envCommandFailure:
		return owned.Error()
	case *envHostRefreshFailure:
		return owned.Error()
	case *configComparisonFailure:
		return owned.Error()
	case setupFailureError:
		return owned.Error()
	case commandPreparationFailure:
		return owned.Error()
	}
	if cleanup, ok := soleCommandOwner(err).(uninstallCleanupError); ok {
		return cleanup.Error()
	}
	if failure.kind == commandUnexpected {
		if _, ok := failure.owner.(managedssh.NativeLaunchError); ok {
			return "The native SSH or transfer tool could not start. Check that the required tool is installed and executable, then retry; run `pb ssh doctor` to check SSH setup."
		}

		if _, ok := failure.owner.(*os.PathError); ok {
			return "Paperboat could not access local files. Check file permissions and the local Paperboat service, then retry; run `pb doctor` if this continues."
		}
		if failure.owner == syscall.EACCES || failure.owner == syscall.EPERM {
			return "Paperboat could not access a local resource. Check permissions and the local Paperboat service, then retry; run `pb doctor` if this continues."
		}
		return generic
	}
	owner := failure.owner
	if soleCommandOwner(err) == nil && failure.kind == commandRejected {
		return generic
	}
	switch owner := owner.(type) {
	case usageError:
		return owner.Error()
	case unsupportedJSONOutputError:
		return "This command does not support JSON output. Run it without --json."
	case *api.APIError:
		if failure.apiCauseInvalid {
			return generic
		}
		if owner.Status == http.StatusUpgradeRequired && owner.Code == "update_required" {
			return owner.Error()
		}
		if owner.Status == http.StatusUnauthorized {
			return "Your Paperboat session is no longer valid. " + dashboardEnrollmentGuidance + " Then retry."
		}
		message := friendlyAPIError(owner)
		if owner.Code == "team_subscription_required" && owner.Status != http.StatusForbidden {
			message = ""
		}
		if message == "" {
			message = apiErrorFallback(owner)
		}
		message = sentence(message)
		if supportref.Valid(owner.SupportReference) {
			message += " Support reference: " + owner.SupportReference + "."
		}
		return message
	case *configComparisonFailure:
		return owner.Error()
	case *envHostRefreshFailure:
		return owner.Error()
	case *envCommandFailure:
		return owner.Error()
	case *localapi.RemoteError:
		return "The local Paperboat service could not complete the request. Check its status with `pb status`, then retry; run `pb doctor` if this continues."
	case *TunnelCreateExistingError:
		return "A tunnel with this name already exists; nothing was changed. Inspect it with `pb tunnel status <tunnel>`."
	case *TunnelCreateChangedError:
		return "The tunnel was created, but a later step failed; the tunnel was preserved. Inspect it with `pb tunnel status <tunnel>` before retrying."
	case *TunnelOperationOutcomeError:
		return "The tunnel operation failed. Inspect its current state with `pb tunnel status <tunnel>` before retrying."
	case *TunnelOperationWaitTimeoutError:
		return "The wait for the tunnel operation timed out. The operation remains active; inspect it with `pb tunnel status <tunnel>`."
	}
	switch failure.kind {
	case commandCanceled, commandInteractiveCanceled:
		return "Operation canceled."
	case commandUsage:
		return "The command arguments are invalid. Run `pb COMMAND --help` and retry."
	case commandDeadline:
		return "The operation timed out. Check the current state before retrying; run `pb doctor` if this continues."
	case commandNativeExit:
		return "The native command did not finish successfully."
	}
	switch failure.owner {
	case localwait.ErrMachineNotFound:
		return "The selected machine was not found. Run `pb status` and select an available machine."
	case localwait.ErrMachineAmbiguous:
		return "The machine selection is ambiguous. Run `pb status` and retry with the machine ID."
	case localwait.ErrInvalid:
		return "The machine wait request is invalid. Run `pb wait --help` and retry."
	case tailnet.ErrAuthority, tailnet.ErrAdmission:
		return "The secure connection could not be established. Retry; if this continues, run `pb doctor`."
	case tunnel.ErrTransportLost:
		return "The terminal connection was lost and could not be restored. Retry `pb`; if this continues, run `pb doctor`."
	case api.ErrUnauthenticated:
		return "Your Paperboat session is no longer valid. " + dashboardEnrollmentGuidance + " Then retry."
	case config.ErrNoCredentials:
		return "Not signed in to Paperboat. " + dashboardEnrollmentGuidance + " Then retry."
	case config.ErrSecretNotFound:
		return "This CLI is signed in but not paired for private transport. Run the enrollment command from the Paperboat dashboard."
	case identitybootstrap.ErrPairingRequired:
		return "This CLI needs approval from a paired machine before private transport can be enabled. Run the enrollment command from the Paperboat dashboard."
	case identitybootstrap.ErrEnrollmentExpired:
		return "Private transport approval expired. Run the enrollment command from the Paperboat dashboard."
	}
	if failure.kind == commandOperational {
		return "Paperboat is unreachable. Check your network connection and retry; if the service is recovering, retry in a moment."
	}
	return generic
}

// A typed result may state mutation facts only when no sibling error exists.
func soleCommandOwner(err error) error {
	for count := 0; err != nil && count < 32; count++ {
		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return nil
		}
		if _, ok := err.(interface{ Unwrap() []error }); ok {
			return nil
		}
		switch err.(type) {
		case *sessionauth.CredentialFailure, commandRejection, *envCommandFailure, *envHostRefreshFailure, *configComparisonFailure, setupFailureError, commandPreparationFailure, *api.APIError, *localapi.RemoteError, *TunnelCreateExistingError, *TunnelCreateChangedError, *TunnelOperationOutcomeError, *TunnelOperationWaitTimeoutError, uninstallCleanupError, preferenceLoadFailure, usageError, unsupportedJSONOutputError, managedssh.NativeExitError, managedssh.NativeLaunchError, exitCodeError, jsonFailureExitCodeError:
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

func statusCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "status [machine]",
		Short: "Show local Paperboat machine status",
		Args:  commandArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {

			_, snapshot, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("read local Paperboat status: %w", err)
			}
			if len(args) == 1 {
				machine, err := selectStatusMachine(snapshot.Machines, args[0])
				if err != nil {
					return err
				}
				snapshot.Machines = []localapi.MachineStatus{machine}
			}
			jsonOutput, _ := command.Flags().GetBool("json")
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				return encoder.Encode(snapshot)
			}
			writeStatus(command.OutOrStdout(), snapshot)
			return nil
		},
	}
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func selectStatusMachine(machines []localapi.MachineStatus, target string) (localapi.MachineStatus, error) {
	return localwait.ResolveMachine(machines, target)
}

func writeStatus(output io.Writer, snapshot localapi.Snapshot) {
	fmt.Fprintf(output, "Daemon: %s  Generation: %d  Observed: %s\n", snapshot.DaemonState, snapshot.Generation, snapshot.ObservedAt.Format(time.RFC3339))
	for _, health := range snapshot.Health {
		fmt.Fprintf(output, "Health: %s: %s  Recovery: %s\n", health.Severity, health.Title, health.Recovery)
	}
	for _, machine := range snapshot.Machines {
		path := machine.SelectedPath
		if machine.RelayRegion != "" {
			path += "/" + machine.RelayRegion
		}
		if len(machine.TransportConsumers) > 0 {
			path = transportConsumerSummary(machine.TransportConsumers)
		}
		observed := "never"
		if machine.LastObservedAt != nil {
			observed = machine.LastObservedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(output, "\n%s (%s)\n", machine.Alias, machine.ID)
		fmt.Fprintf(output, "  Runtime: %s  Eligible: %t  Generation: %d  Last observed: %s\n", machine.RuntimeState, machine.Eligible, machine.Generation, observed)
		fmt.Fprintf(output, "  Path: %s  Consumers: %d  Transfer: %s  Preview: %s  SSH: %s\n", path, machine.ActiveConsumers, machine.TransferReadiness, machine.PreviewReadiness, machine.SSHReadiness)
		for _, health := range machine.Health {
			fmt.Fprintf(output, "  Health: %s: %s  Recovery: %s\n", health.Severity, health.Title, health.Recovery)
		}
	}
}

func transportConsumerSummary(consumers []localapi.TransportConsumer) string {
	parts := make([]string, 0, len(consumers))
	for _, consumer := range consumers {
		path := consumer.Path
		if consumer.RelayRegion != "" {
			path += "/" + consumer.RelayRegion
		}
		parts = append(parts, fmt.Sprintf("%s=%d", path, consumer.ActiveConsumers))
	}
	return strings.Join(parts, ", ")
}

const (
	doctorRunTimeout              = 20 * time.Second
	doctorProbeTimeout            = 10 * time.Second
	doctorPathReachabilityTimeout = 8 * time.Second
)

func doctorCommandV1() *cobra.Command {
	command := &cobra.Command{
		Use:   "doctor [machine]",
		Short: "Check Paperboat connectivity and readiness",
		Args:  commandArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			if supportref.FromContext(command.Context()) == "" {
				command.SetContext(supportref.WithContext(command.Context(), supportref.New()))
			}
			repair, _ := command.Flags().GetBool("repair")
			if repair {
				if len(args) != 0 {
					return localArgumentError("doctor --repair does not accept a machine")
				}
				if runtime.GOOS == "windows" {
					doctorArgs := []string{"doctor", "--repair"}
					if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
						doctorArgs = append(doctorArgs, "--json")
					}
					if code := hostruntimecmd.Execute(command.Context(), doctorArgs, command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr()); code != 0 {
						return exitCodeError{code: code}
					}
					return nil
				}
				return repairWindowsOpenSSH(command.Context())
			}
			_, snapshot, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("read local Paperboat diagnostics: %w", err)
			}
			var machine *localapi.MachineStatus
			var reportMachine *doctorpkg.Machine
			if len(args) == 1 {
				selected, selectErr := selectStatusMachine(snapshot.Machines, args[0])
				if selectErr != nil {
					return selectErr
				}
				machine = &selected
				reportMachine = &doctorpkg.Machine{ID: selected.ID, Alias: selected.Alias}
			}
			report, err := doctorpkg.Run(command.Context(), doctorpkg.Config{
				Timeout: doctorRunTimeout, ProbeTimeout: doctorProbeTimeout, Clock: time.Now,
				Correlation: func() (string, error) { return supportref.FromContext(command.Context()), nil },
			}, reportMachine, doctorProbes(command, snapshot, machine))
			if err != nil {
				return err
			}
			jsonOutput, _ := command.Flags().GetBool("json")
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(report); err != nil {
					return err
				}
			} else {
				writeDoctorReport(command.OutOrStdout(), report)
			}
			if report.Overall != "healthy" {
				return exitCodeError{code: 1}
			}
			return nil
		},
	}
	command.Flags().Bool("json", false, "print JSON")
	command.Flags().Bool("repair", false, "repair Paperboat-owned local dependencies")
	return command
}

func doctorProbes(command *cobra.Command, snapshot localapi.Snapshot, machine *localapi.MachineStatus) []doctorpkg.Probe {
	probes := []doctorpkg.Probe{
		{Code: "authentication", Run: func(ctx context.Context) doctorpkg.Check { return probeDoctorAuthentication(ctx, command) }},
		{Code: "daemon", Run: func(context.Context) doctorpkg.Check {
			status := doctorpkg.StatusPass
			recovery := ""
			if snapshot.DaemonState != "ready" {
				status = doctorpkg.StatusWarning
				recovery = "Restart the Paperboat local service and run pb doctor again."
			}
			return doctorpkg.Check{Category: "local", Code: "daemon", Status: status, Summary: "The local daemon is " + snapshot.DaemonState + ".", Recovery: recovery}
		}},
		{Code: "local_state", Run: func(ctx context.Context) doctorpkg.Check { return probeDoctorLocalState(ctx) }},
		{Code: "udp_ipv4", Run: func(ctx context.Context) doctorpkg.Check {
			return probeDoctorUDP(ctx, "udp4", "0.0.0.0:0", "udp_ipv4", "IPv4")
		}},
		{Code: "udp_ipv6", Run: func(ctx context.Context) doctorpkg.Check {
			return probeDoctorUDP(ctx, "udp6", "[::]:0", "udp_ipv6", "IPv6")
		}},
	}
	if machine != nil {
		probes = append(probes, doctorMachineProbes(*machine)...)
		probes = append(probes, doctorPathReachabilityProbes(command, machine.ID)...)
	}
	return probes
}

func doctorPathReachabilityProbes(command *cobra.Command, machineID string) []doctorpkg.Probe {
	return doctorNativeReachabilityProbe(command.Context(), doctorPathReachabilityTimeout, func(ctx context.Context) (tunnel.NativeProbe, error) {
		commandContext := actionContext(command, []string{machineID})
		commandContext.Context = ctx
		dependencies, err := buildDeps(commandContext)
		if err != nil {
			return tunnel.NativeProbe{}, err
		}
		if dependencies.peerLocal == nil {
			return tunnel.NativeProbe{}, errors.New("peer tunnel is unavailable")
		}
		client, err := backendClient(commandContext)
		if err != nil {
			return tunnel.NativeProbe{}, err
		}
		machine, err := resolveUserMachine(ctx, client, machineID)
		if err != nil {
			return tunnel.NativeProbe{}, err
		}
		if !machine.Online {
			return tunnel.NativeProbe{}, errors.New("selected machine is offline")
		}
		target := resolver.ConnectInfo{TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
		return probeDaemonPeer(ctx, dependencies.peerLocal, target, "doctor_native")
	})
}

func doctorNativeReachabilityProbe(parent context.Context, timeout time.Duration, load func(context.Context) (tunnel.NativeProbe, error)) []doctorpkg.Probe {
	return []doctorpkg.Probe{{Code: "peer_reachability", Run: func(probeCtx context.Context) doctorpkg.Check {
		if parent == nil || probeCtx == nil || timeout <= 0 || load == nil {
			return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusUnavailable, Summary: "Native peer reachability could not be checked.", Recovery: "Check the local Paperboat service and run pb doctor again."}
		}
		ctx, cancel := context.WithTimeout(probeCtx, timeout)
		defer cancel()
		stopParentCancellation := context.AfterFunc(parent, cancel)
		defer stopParentCancellation()
		if parent.Err() != nil {
			cancel()
		}
		result, err := load(ctx)
		if ctx.Err() != nil {
			cause := err
			if cause == nil {
				cause = ctx.Err()
			} else if cause != ctx.Err() {
				cause = errors.Join(cause, ctx.Err())
			}
			if !errorreport.HTTPAttemptObserved(cause) {
				errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "peer_connect", "command_failed", cause)
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusUnavailable, Summary: "Native peer reachability check timed out.", Recovery: "Check the selected machine and network, then run pb doctor again."}
			}
			return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusUnavailable, Summary: "Native peer reachability check was canceled.", Recovery: "Run pb doctor again when the selected machine and network are available."}
		}
		if err != nil {
			errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "peer_connect", "command_failed", err)
			return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusFail, Summary: "Could not establish an authenticated native connection to the selected machine.", Recovery: "Check the Paperboat service on the selected machine and local network access, then run pb doctor again."}
		}
		return doctorPeerCheck(result)
	}}}
}

func doctorPeerCheck(result tunnel.NativeProbe) doctorpkg.Check {
	check := doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusPass, Summary: "Established an authenticated native connection to the selected machine.", SelectedPath: result.Path}
	switch result.Path {
	case "direct":
	case "peer_relay", "regional_relay":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "Established an authenticated native connection through a relay."
		check.Recovery = "Paperboat will retry direct connectivity automatically; check UDP availability if relay use persists."
	case "unknown":
		check.Summary = "Established an authenticated native connection; its underlay path is not currently observed."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Native connection path could not be classified."
	}
	return check
}

func probeDoctorAuthentication(ctx context.Context, command *cobra.Command) doctorpkg.Check {
	check := doctorpkg.Check{Category: "control", Code: "authentication", Status: doctorpkg.StatusFail, Summary: "Paperboat authentication is unavailable.", Recovery: "Check local credential storage and the Paperboat service, then run `pb doctor` again."}
	cfg, err := config.Load(configPathFlag(command))
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "command", "command_failed", err)
		check.Summary = "The Paperboat configuration could not be loaded."
		check.Recovery = "Repair the Paperboat configuration and run pb doctor again."
		return check
	}
	if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(server)
		if err != nil {
			check.Summary = "The configured Paperboat server URL is invalid."
			check.Recovery = "Correct the Paperboat server URL and run pb doctor again."
			return check
		}
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		check.Summary = "The Paperboat server is not configured."
		check.Recovery = "Configure the Paperboat server and run pb doctor again."
		return check
	}
	auth, err := sessionauth.NewSource(cfg)
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "command", "command_failed", err)
		return check
	}
	credential, err := auth.Credential()
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "command", "command_failed", err)
		if onlyEnvironmentFailureLeaves(err, func(leaf error) bool {
			return leaf == config.ErrNoCredentials || leaf == config.ErrSecretNotFound || leaf == api.ErrUnauthenticated
		}) {
			check.Recovery = dashboardEnrollmentGuidance + " Then run `pb doctor` again."
		}
		return check
	}
	if _, err := api.New(cfg.ServerURL, credential, nil).Me(ctx); err != nil {
		if diagnosis, ok := doctorProxyDiagnosis(err); ok {
			check.Summary = "The Paperboat server is unreachable through the configured proxy."
			check.Recovery = diagnosis.Recovery
		} else {
			check.Summary = "The Paperboat server did not accept the authenticated health check."
			check.Recovery = "Check network access and sign in again if needed, then run pb doctor again."
		}
		return check
	}
	return doctorpkg.Check{Category: "control", Code: "authentication", Status: doctorpkg.StatusPass, Summary: "The Paperboat server accepted the current credential."}
}

func probeDoctorLocalState(ctx context.Context) doctorpkg.Check {
	state := collectLocalDoctor(ctx)
	if state.SetupState == "configured" && state.IdentityState == "valid" {
		return doctorpkg.Check{Category: "local", Code: "local_state", Status: doctorpkg.StatusPass, Summary: "Local Paperboat state and identity are valid."}
	}
	recovery := "Run pb setup, then run pb doctor again."
	if len(state.RecoveryActions) > 0 {
		recovery = sentence(state.RecoveryActions[0])
	}
	return doctorpkg.Check{Category: "local", Code: "local_state", Status: doctorpkg.StatusWarning, Summary: "Local Paperboat runtime state is not fully configured.", Recovery: recovery}
}

func probeDoctorUDP(ctx context.Context, network, address, code, family string) doctorpkg.Check {
	packet, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "listener_bind", "command_failed", err)
		return doctorpkg.Check{Category: "network", Code: code, Status: doctorpkg.StatusWarning, Summary: family + " UDP sockets are unavailable.", Recovery: "Check local firewall and network settings, then run pb doctor again."}
	}
	return finishDoctorUDP(ctx, packet, code, family)
}

func finishDoctorUDP(ctx context.Context, packet io.Closer, code, family string) doctorpkg.Check {
	if err := packet.Close(); err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "listener_bind", "command_failed", err)
		return doctorpkg.Check{Category: "network", Code: code, Status: doctorpkg.StatusWarning, Summary: family + " UDP socket cleanup failed.", Recovery: "Check the local Paperboat service, then run pb doctor again."}
	}
	return doctorpkg.Check{Category: "network", Code: code, Status: doctorpkg.StatusPass, Summary: family + " UDP sockets are available."}
}

func doctorMachineProbes(machine localapi.MachineStatus) []doctorpkg.Probe {
	return []doctorpkg.Probe{
		{Code: "machine_runtime", Run: func(context.Context) doctorpkg.Check {
			if machine.Eligible && machine.RuntimeState == "ready" {
				return doctorpkg.Check{Category: "machine", Code: "machine_runtime", Status: doctorpkg.StatusPass, Summary: "The selected machine runtime is ready."}
			}
			return doctorpkg.Check{Category: "machine", Code: "machine_runtime", Status: doctorpkg.StatusFail, Summary: "The selected machine runtime is not ready.", Recovery: "Check the Paperboat service on the selected machine and run pb doctor again."}
		}},
		{Code: "selected_path", Run: func(context.Context) doctorpkg.Check {
			switch machine.SelectedPath {
			case "direct":
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusPass, Summary: "The active Paperboat path is direct."}
			case "relay":
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusWarning, Summary: "The active Paperboat path uses a regional relay.", Recovery: "Direct connectivity is retried automatically; check UDP availability if relay use persists."}
			case "wss":
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusWarning, Summary: "The active Paperboat path uses WebSocket fallback.", Recovery: "Check UDP and QUIC access; Paperboat will retry stronger paths automatically."}
			default:
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusUnavailable, Summary: "No active Paperboat path is currently observed."}
			}
		}},
		{Code: "nat_mapping_ipv4", Run: func(context.Context) doctorpkg.Check { return doctorNATCheck("ipv4", machine.NATMappingIPv4) }},
		{Code: "nat_mapping_ipv6", Run: func(context.Context) doctorpkg.Check { return doctorNATCheck("ipv6", machine.NATMappingIPv6) }},
		{Code: "captive_portal", Run: func(context.Context) doctorpkg.Check { return doctorCaptivePortalCheck(machine.CaptivePortal) }},
		{Code: "path_mtu", Run: func(context.Context) doctorpkg.Check { return doctorPMTUCheck(machine.PMTU) }},
		{Code: "router_protocol", Run: func(context.Context) doctorpkg.Check { return doctorRouterProtocolCheck(machine.RouterProtocol) }},
		{Code: "router_mapping", Run: func(context.Context) doctorpkg.Check { return doctorRouterMappingCheck(machine.RouterMapping) }},
		{Code: "mapping_lifetime", Run: func(context.Context) doctorpkg.Check { return doctorMappingLifetimeCheck(machine.MappingLifetime) }},
		{Code: "update_health", Run: func(context.Context) doctorpkg.Check { return doctorUpdateHealthCheck(machine.UpdateHealth) }},
		{Code: "ssh_readiness", Run: func(context.Context) doctorpkg.Check {
			switch machine.SSHReadiness {
			case "ready":
				return doctorpkg.Check{Category: "ssh", Code: "ssh_readiness", Status: doctorpkg.StatusPass, Summary: "Managed SSH is ready."}
			case "degraded":
				return doctorpkg.Check{Category: "ssh", Code: "ssh_readiness", Status: doctorpkg.StatusWarning, Summary: "Managed SSH requires attention.", Recovery: "Run pb ssh doctor for the selected machine."}
			default:
				return doctorpkg.Check{Category: "ssh", Code: "ssh_readiness", Status: doctorpkg.StatusUnavailable, Summary: "Managed SSH readiness is unavailable."}
			}
		}},
	}
}

func doctorRouterProtocolCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "router_protocol"}
	switch value {
	case "pcp":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The verified router mapping uses PCP."
	case "nat_pmp":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The verified router mapping uses NAT-PMP."
	case "upnp":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The verified router mapping uses UPnP."
	case "none":
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "This network did not provide a router mapping protocol."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "The router mapping protocol could not be observed."
	}
	return check
}

func doctorMappingLifetimeCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "mapping_lifetime"}
	switch value {
	case "30s_to_2m", "2m_to_10m", "over_10m":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The authenticated UDP mapping lifetime supports adaptive keepalive."
	case "under_30s":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "The authenticated UDP mapping lifetime is short."
		check.Recovery = "Keep relay fallback available; check router UDP timeout settings if direct connections are unstable."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "An authenticated UDP mapping lifetime measurement is not available."
	}
	return check
}

func doctorRouterMappingCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "router_mapping"}
	switch value {
	case "verified":
		check.Status = doctorpkg.StatusPass
		check.Summary = "A router mapping was verified from the owned direct-path socket."
	case "unreachable":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "A router mapping could not be verified from outside the local network."
		check.Recovery = "Check router and firewall UDP policy; relay fallback remains available."
	case "untrusted":
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Router mapping is disabled on this network by Paperboat policy."
	case "unavailable":
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "This network does not provide a usable router mapping."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Router mapping status could not be observed."
	}
	return check
}

func doctorUpdateHealthCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "update", Code: "update_health"}
	switch value {
	case "healthy":
		check.Status = doctorpkg.StatusPass
		check.Summary = "Signed update health is ready."
	case "recovery_required":
		check.Status = doctorpkg.StatusFail
		check.Summary = "Signed update recovery requires attention."
		check.Recovery = "Restart the Paperboat services; if the failure remains, reinstall the current signed release."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Signed update health is unavailable."
	}
	return check
}

func doctorNATCheck(family, mapping string) doctorpkg.Check {
	base := doctorpkg.Check{Category: "network", Code: "nat_mapping_" + family}
	switch mapping {
	case "endpoint_independent":
		base.Status = doctorpkg.StatusPass
		base.Summary = "The observed " + family + " NAT mapping is endpoint-independent."
	case "destination_dependent":
		base.Status = doctorpkg.StatusWarning
		base.Summary = "The observed " + family + " NAT mapping depends on the destination."
		base.Recovery = "Keep relay fallback available and run pb doctor again from another network."
	default:
		base.Status = doctorpkg.StatusUnavailable
		base.Summary = "The " + family + " NAT mapping could not be observed."
	}
	return base
}

func doctorCaptivePortalCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "captive_portal"}
	switch value {
	case "clear":
		check.Status = doctorpkg.StatusPass
		check.Summary = "No captive portal was observed."
	case "suspected":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "This network may require captive portal sign-in."
		check.Recovery = "Complete the network sign-in, then run pb doctor again."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Captive portal status could not be observed."
	}
	return check
}

func doctorPMTUCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "path_mtu"}
	switch value {
	case "standard", "extended":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The authenticated path MTU is suitable for direct QUIC."
	case "minimum_1200":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "The authenticated path supports only the minimum QUIC packet size."
		check.Recovery = "Check VPN and router MTU settings; relay fallback remains available."
	case "below_quic_floor":
		check.Status = doctorpkg.StatusFail
		check.Summary = "The authenticated path MTU is below the QUIC minimum."
		check.Recovery = "Check VPN and router MTU settings, then run pb doctor again."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "An authenticated path MTU measurement is not available."
	}
	return check
}

func writeDoctorReport(output io.Writer, report doctorpkg.Report) {
	fmt.Fprintf(output, "Paperboat doctor: %s\n", report.Overall)
	fmt.Fprintf(output, "Correlation: %s\n", report.CorrelationID)
	if report.Machine != nil {
		fmt.Fprintf(output, "Machine: %s (%s)\n", report.Machine.Alias, report.Machine.ID)
	}
	for _, check := range report.Checks {
		fmt.Fprintf(output, "%s: %s: %s\n", check.Code, check.Status, check.Summary)
		if check.Recovery != "" {
			fmt.Fprintf(output, "  Recovery: %s\n", check.Recovery)
		}
	}
}

func bugreportCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "bugreport",
		Short: "Create a redacted Paperboat diagnostic bundle",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			record, _ := command.Flags().GetBool("record")
			upload, _ := command.Flags().GetBool("upload")
			localClient, _, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("start local Paperboat daemon: %w", err)
			}
			var server bugreportpkg.Server
			if upload {
				ctx := actionContext(command, nil)
				d, depErr := buildDeps(ctx)
				if depErr != nil {
					return depErr
				}
				server, err = newRefreshingBugreportServer(d.cfg.ServerURL, d.auth)
				if err != nil {
					return err
				}
			}
			promptOutput := command.ErrOrStderr()
			beforeUpload := func(result bugreportpkg.Result) error {
				_, writeErr := fmt.Fprintf(command.ErrOrStderr(), "Uploading redacted categories: %s (%d bytes)\n", strings.Join(result.Categories, ", "), result.Bytes)
				return writeErr
			}
			if jsonOutput {
				promptOutput = io.Discard
				beforeUpload = func(bugreportpkg.Result) error { return nil }
			}
			result, runErr := bugreportpkg.Run(command.Context(), bugreportpkg.Options{
				Record: record, Upload: upload, Input: command.InOrStdin(), Prompt: promptOutput,
				Local: localClient, Server: server,
				BeforeUpload: beforeUpload,
			})
			if jsonOutput {
				if runErr != nil {
					result.Error = bugreportJSONFailure(runErr, result.BundleCreated)
					result.Error.SupportReference = supportref.FromContext(command.Context())
					var remote *api.APIError
					if errors.As(runErr, &remote) && supportref.Valid(remote.SupportReference) {
						result.Error.SupportReference = remote.SupportReference
					}
				}
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				if encodeErr := encoder.Encode(result); encodeErr != nil {
					return encodeErr
				}
				if runErr != nil {
					return presentedCommandFailure{cause: runErr}
				}
				return nil
			}
			if runErr != nil {
				if result.BundleCreated {
					return fmt.Errorf("%w; local bundle remains at %s", runErr, result.BundlePath)
				}
				return runErr
			}
			writeBugreportResult(command.OutOrStdout(), result)
			return nil
		},
	}
	command.Flags().Bool("record", false, "record reproduction start and end markers")
	command.Flags().Bool("upload", false, "upload the exact redacted bundle")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

type refreshingBugreportServer struct {
	serverURL string
	auth      config.AuthSource
	client    *api.Client
}

func newRefreshingBugreportServer(serverURL string, auth config.AuthSource) (*refreshingBugreportServer, error) {
	credential, err := auth.Credential()
	if err != nil {
		return nil, err
	}
	return &refreshingBugreportServer{serverURL: serverURL, auth: auth, client: api.New(serverURL, credential, nil)}, nil
}

func (s *refreshingBugreportServer) refresh() (bool, error) {
	refresher, ok := s.auth.(interface {
		Refresh() (config.Credential, error)
	})
	if !ok {
		return false, nil
	}
	credential, err := refresher.Refresh()
	if err != nil {
		return false, err
	}
	s.client = api.New(s.serverURL, credential, nil)
	return true, nil
}

func (s *refreshingBugreportServer) CreateDiagnosticUploadIntent(ctx context.Context, key string, request api.DiagnosticUploadIntentRequest) (api.DiagnosticUploadIntent, error) {
	result, err := s.client.CreateDiagnosticUploadIntent(ctx, key, request)
	if errors.Is(err, api.ErrUnauthenticated) {
		refreshed, refreshErr := s.refresh()
		if refreshErr != nil {
			return result, refreshErr
		}
		if refreshed {
			return s.client.CreateDiagnosticUploadIntent(ctx, key, request)
		}
	}
	return result, err
}

func (s *refreshingBugreportServer) UploadDiagnosticBundle(ctx context.Context, intent api.DiagnosticUploadIntent, content io.Reader, bytes int64) error {
	return s.client.UploadDiagnosticBundle(ctx, intent, content, bytes)
}

func (s *refreshingBugreportServer) CompleteDiagnosticUploadIntent(ctx context.Context, intentID string) (api.DiagnosticUploadIntent, error) {
	result, err := s.client.CompleteDiagnosticUploadIntent(ctx, intentID)
	if errors.Is(err, api.ErrUnauthenticated) {
		refreshed, refreshErr := s.refresh()
		if refreshErr != nil {
			return result, refreshErr
		}
		if refreshed {
			return s.client.CompleteDiagnosticUploadIntent(ctx, intentID)
		}
	}
	return result, err
}

func writeBugreportResult(output io.Writer, result bugreportpkg.Result) {
	fmt.Fprintf(output, "Bundle: %s\n", result.BundlePath)
	fmt.Fprintf(output, "Categories: %s\n", strings.Join(result.Categories, ", "))
	fmt.Fprintf(output, "Size: %d bytes\n", result.Bytes)
	if result.Uploaded {
		fmt.Fprintf(output, "Server correlation: %s\n", result.ServerCorrelationID)
	}
}

func bugreportJSONFailure(err error, bundleCreated bool) *bugreportpkg.Failure {
	stage := "bugreport"
	var stageErr *bugreportpkg.StageError
	if errors.As(err, &stageErr) {
		stage = stageErr.Stage
	}
	code, message := "bugreport_failed", "The diagnostic bundle could not be created."
	if bundleCreated {
		code, message = "bugreport_upload_failed", "The operation did not finish; the local redacted bundle was preserved."
	}
	return &bugreportpkg.Failure{Code: code, Stage: stage, Message: message}
}

func waitCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "wait <machine>",
		Short: "Wait for a machine readiness condition",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			condition, _ := command.Flags().GetString("for")
			if condition != "runtime" && condition != "transport" && condition != "ssh" {
				return invocationError(errors.New("--for must be runtime, transport, or ssh"))
			}
			timeout, _ := command.Flags().GetDuration("timeout")
			if timeout <= 0 || timeout > 24*time.Hour {
				return invocationError(errors.New("--timeout must be greater than zero and no more than 24h"))
			}
			client, snapshot, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("start local Paperboat daemon: %w", err)
			}
			waitCtx, cancel := context.WithTimeout(command.Context(), timeout)
			defer cancel()
			var result localwait.Result
			if condition == "runtime" {
				result, err = localwait.WaitTargetFromSnapshot(waitCtx, client, snapshot, args[0], condition)
			} else {
				result, err = waitForAuthenticatedTransport(waitCtx, command, client, snapshot, args[0], condition)
			}
			if err != nil {
				return fmt.Errorf("wait for local Paperboat status: %w", err)
			}
			jsonOutput, _ := command.Flags().GetBool("json")
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(result); err != nil {
					return err
				}
			} else {
				writeWaitResult(command.OutOrStdout(), command.ErrOrStderr(), result)
			}
			if code := waitExitCode(result); code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
	}
	command.Flags().String("for", "transport", "readiness condition: runtime, transport, or ssh")
	command.Flags().Duration("timeout", 5*time.Minute, "maximum time to wait")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func waitForAuthenticatedTransport(ctx context.Context, command *cobra.Command, client *localapi.Client, snapshot localapi.Snapshot, target, condition string) (localwait.Result, error) {
	dependencies, err := buildDeps(actionContext(command, []string{target}))
	if err != nil {
		return localwait.Result{}, err
	}
	if dependencies.peerLocal == nil {
		return localwait.Result{}, errors.New("authenticated peer transport is unavailable")
	}
	backend, err := backendClient(actionContext(command, []string{target}))
	if err != nil {
		return localwait.Result{}, err
	}
	for {
		localMachine, resolveErr := localwait.ResolveMachine(snapshot.Machines, target)
		if resolveErr != nil {
			return localwait.Result{}, resolveErr
		}
		ready := localMachine.Eligible && localMachine.Generation > 0 && (localMachine.RuntimeState == "ready" || localMachine.RuntimeState == "degraded")
		if condition == "ssh" {
			ready = ready && localMachine.SSHReadiness == "ready"
		}
		if ready {
			machine, machineErr := resolveUserMachine(ctx, backend, localMachine.ID)
			if machineErr == nil && machine.Online {
				info := resolver.ConnectInfo{TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
				probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
				probe, probeErr := probeDaemonPeer(probeCtx, client, info, "wait_transport")
				cancelProbe()
				if probeErr == nil {
					for index := range snapshot.Machines {
						if snapshot.Machines[index].ID == localMachine.ID {
							snapshot.Machines[index].SelectedPath = nativeSnapshotPath(probe.Path)
							snapshot.Machines[index].RelayRegion = ""
						}
					}
					return localwait.WaitTargetFromSnapshot(ctx, client, snapshot, localMachine.ID, condition)
				}
				if errors.Is(probeErr, localapi.ErrPermission) {
					return localwait.Result{}, probeErr
				}
			}
		}
		select {
		case <-ctx.Done():
			return localwait.WaitTargetFromSnapshot(ctx, client, snapshot, localMachine.ID, condition)
		case <-time.After(time.Second):
		}
		snapshot, err = client.Snapshot(ctx)
		if err != nil {
			return localwait.Result{}, err
		}
	}
}

type localDaemonServiceInstaller func(context.Context, string, string, string) error
type localDaemonServiceRemover func(context.Context, string) error
type localDaemonSnapshotClient interface {
	Snapshot(context.Context) (localapi.Snapshot, error)
}
type localDaemonSnapshotLauncher func(context.Context) error

const (
	localDaemonReadyTimeout      = 30 * time.Second
	localDaemonReadyPollInterval = 50 * time.Millisecond
)

var localDaemonLaunchGate = make(chan struct{}, 1)

// localDaemonRegistrationMatches reports whether this machine has a machine
// registration for the active server. A clean sign-in intentionally has no
// registration yet: auth must not launch a daemon that can only exit with a
// setup-required error.
func localDaemonRegistrationMatches(cfg *config.Config) (bool, error) {
	if cfg == nil || strings.TrimSpace(cfg.ServerURL) == "" {
		return false, localdaemon.ErrInvalidInventoryConfig
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return false, err
	}
	registration, err := store.Registration()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	configuredServer, err := config.NormalizeServerURL(cfg.ServerURL)
	if err != nil {
		return false, err
	}
	registeredServer, err := config.NormalizeServerURL(registration.ServerURL)
	if err != nil {
		return false, err
	}
	return registration.MachineID != "" && registeredServer == configuredServer, nil
}

func localDaemonLaunchValues(cfg *config.Config) (string, string, string, error) {
	if cfg == nil || strings.TrimSpace(cfg.ServerURL) == "" {
		return "", "", "", localdaemon.ErrInvalidInventoryConfig
	}
	executable, err := os.Executable()
	if err != nil {
		return "", "", "", err
	}
	if runtime.GOOS == "windows" {
		// The owner-scoped SCM service uses its persisted installation authority.
		// CLI config/server overrides are not service launch arguments.
		return executable, "", "", nil
	}
	configPath := cfg.Path()
	if configPath != "" {
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			return "", "", "", err
		}
	}
	return executable, configPath, cfg.ServerURL, nil
}

func waitForLocalDaemonSnapshot(ctx context.Context, client localDaemonSnapshotClient, socketPath string, lastSocketErr error) (localapi.Snapshot, error) {
	if ctx == nil || client == nil || strings.TrimSpace(socketPath) == "" {
		return localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	ticker := time.NewTicker(localDaemonReadyPollInterval)
	defer ticker.Stop()
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil {
			return snapshot, nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			if lastSocketErr == nil && localDaemonSocketUnavailable(err) {
				lastSocketErr = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
			}
			if lastSocketErr != nil {
				return localapi.Snapshot{}, errors.Join(lastSocketErr, contextErr)
			}
			return localapi.Snapshot{}, errors.Join(err, contextErr)
		}
		if !localDaemonSocketUnavailable(err) {
			return localapi.Snapshot{}, err
		}
		lastSocketErr = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
		select {
		case <-ctx.Done():
			return localapi.Snapshot{}, errors.Join(lastSocketErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func acquireLocalDaemonLaunch(ctx context.Context) (func(), error) {
	select {
	case localDaemonLaunchGate <- struct{}{}:
		return func() { <-localDaemonLaunchGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func recoverLocalDaemonSnapshot(ctx context.Context, client localDaemonSnapshotClient, socketPath string, launch localDaemonSnapshotLauncher) (localapi.Snapshot, error) {
	if ctx == nil || client == nil || launch == nil || strings.TrimSpace(socketPath) == "" {
		return localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	snapshot, err := client.Snapshot(ctx)
	if err == nil {
		return snapshot, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		if localDaemonSocketUnavailable(err) {
			err = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
		}
		return localapi.Snapshot{}, errors.Join(err, contextErr)
	}
	if !localDaemonSocketUnavailable(err) {
		return localapi.Snapshot{}, err
	}
	lastSocketErr := fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
	release, err := acquireLocalDaemonLaunch(ctx)
	if err != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, err)
	}
	defer release()

	snapshot, err = client.Snapshot(ctx)
	if err == nil {
		return snapshot, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, contextErr)
	}
	if !localDaemonSocketUnavailable(err) {
		return localapi.Snapshot{}, err
	}
	lastSocketErr = fmt.Errorf("connect local daemon at %s: %w", socketPath, err)
	if err := launch(ctx); err != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, err)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return localapi.Snapshot{}, errors.Join(lastSocketErr, contextErr)
	}
	return waitForLocalDaemonSnapshot(ctx, client, socketPath, lastSocketErr)
}

// startLocalDaemonAndWait first accepts an already-ready owner daemon. When
// the local endpoint is absent it installs the service and waits for one valid
// snapshot, so process creation cannot be mistaken for readiness.
func startLocalDaemonAndWait(ctx context.Context, cfg *config.Config, install localDaemonServiceInstaller) (*localapi.Client, localapi.Snapshot, error) {
	return startLocalDaemonAndWaitWithin(ctx, cfg, install, localDaemonReadyTimeout)
}

func startLocalDaemonAndWaitWithin(ctx context.Context, cfg *config.Config, install localDaemonServiceInstaller, timeout time.Duration) (*localapi.Client, localapi.Snapshot, error) {
	if install == nil {
		return nil, localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return nil, localapi.Snapshot{}, localdaemon.ErrInvalidInventoryConfig
	}
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	snapshot, err := recoverLocalDaemonSnapshot(operationCtx, client, paths.SocketPath, func(launchCtx context.Context) error {
		executable, configPath, server, launchErr := localDaemonLaunchValues(cfg)
		if launchErr != nil {
			return launchErr
		}
		return install(launchCtx, executable, configPath, server)
	})
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	return client, snapshot, nil
}

// ensureLocalDaemonService is used by auth. Before the first setup it is a
// deliberate no-op; setup owns the first machine-bound daemon launch.
func ensureLocalDaemonService(ctx context.Context, cfg *config.Config) error {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil || !matches {
		return err
	}
	_, _, err = startLocalDaemonAndWait(ctx, cfg, installLocalDaemonService)
	return err
}

// requireLocalDaemonService is the bounded recovery path for peer commands.
// It covers reboot followed only by a non-interactive Windows OpenSSH logon,
// where the managed local daemon service is temporarily cold.
func requireLocalDaemonService(ctx context.Context, cfg *config.Config) error {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("this machine is not set up for the active Paperboat server; run `pb setup`")
	}
	_, _, err = startLocalDaemonAndWait(ctx, cfg, installLocalDaemonService)
	return err
}

// rebindLocalDaemonService restarts the owner daemon only after setup has
// committed the new machine registration. This prevents a still-running
// daemon from retaining a previous machine or server identity.
func rebindLocalDaemonService(ctx context.Context, cfg *config.Config) error {
	matches, err := localDaemonRegistrationMatches(cfg)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("Client machine registration is unavailable for the active Paperboat server")
	}
	executable, _, _, err := localDaemonLaunchValues(cfg)
	if err != nil {
		return err
	}
	if err := removeLocalDaemonService(ctx, executable); err != nil {
		return fmt.Errorf("stop previous local daemon: %w", err)
	}
	_, _, err = startLocalDaemonAndWait(ctx, cfg, installLocalDaemonService)
	return err
}

func localDaemonSnapshot(command *cobra.Command, install localDaemonServiceInstaller) (*localapi.Client, localapi.Snapshot, error) {
	if command == nil || install == nil {
		return nil, localapi.Snapshot{}, errors.New("invalid local daemon client configuration")
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	commandCtx := command.Context()
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	operationCtx, cancel := context.WithTimeout(commandCtx, localDaemonReadyTimeout)
	defer cancel()
	snapshot, err := recoverLocalDaemonSnapshot(operationCtx, client, paths.SocketPath, func(launchCtx context.Context) error {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			return executableErr
		}
		configPath := configPathFlag(command)
		effectiveConfig, configErr := config.Load(configPath)
		if configErr != nil {
			return configErr
		}
		configPath = effectiveConfig.Path()
		if configPath != "" {
			configPath, executableErr = filepath.Abs(configPath)
			if executableErr != nil {
				return executableErr
			}
		}
		server, _ := command.Flags().GetString("server")
		if strings.TrimSpace(server) == "" {
			server = effectiveConfig.ServerURL
		}
		return install(launchCtx, executable, configPath, server)
	})
	if err != nil {
		return nil, localapi.Snapshot{}, err
	}
	return client, snapshot, nil
}

func writeWaitResult(stdout, stderr io.Writer, result localwait.Result) {
	switch result.Outcome {
	case "ready":
		fmt.Fprintf(stdout, "%s is ready for %s (%s, generation %d).\n", result.Machine.Alias, result.Condition, result.Machine.RuntimeState, result.SnapshotGeneration)
	case "timeout":
		fmt.Fprintf(stderr, "pb: Timed out waiting for %s to become ready for %s.\n", result.Machine.Alias, result.Condition)
	case "canceled":
		fmt.Fprintln(stderr, "pb: Operation canceled.")
	case "failed":
		fmt.Fprintf(stderr, "pb: %s cannot become ready for %s (%s).\n", result.Machine.Alias, result.Condition, result.Code)
	}
}

func waitExitCode(result localwait.Result) int {
	switch result.Outcome {
	case "ready":
		return 0
	case "timeout":
		return 203
	case "canceled":
		return 205
	default:
		return 1
	}
}

func privilegedServiceOperationCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "__runtime-service",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(command *cobra.Command, args []string) error {
			code := hostruntimecmd.Execute(command.Context(), append([]string{"service"}, args...), command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
			if code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func pairCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "pair",
		Short: "Enroll this machine with a one-shot token",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			stateRoot, err := command.Flags().GetString("state-root")
			if err != nil {
				return err
			}
			if stateRoot == "" {
				stateRoot = os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
			}
			if stateRoot == "" {
				stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
				if err != nil {
					return err
				}
			}
			identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
			if err != nil {
				return fmt.Errorf("open machine identity: %w", err)
			}
			registration, err := identityStore.Registration()
			fresh := errors.Is(err, os.ErrNotExist)
			if fresh {
				registration = identity.Registration{}
			}
			if err != nil {
				if fresh {
					err = nil
				} else {
					return fmt.Errorf("load machine registration: %w", err)
				}
			}
			serverURL, err := command.Flags().GetString("server")
			if err != nil {
				return err
			}
			if serverURL != "" {
				serverURL, err = config.NormalizeServerURL(serverURL)
				if err != nil {
					return err
				}
				if !fresh && serverURL != registration.ServerURL {
					return errors.New("this machine is set up for a different Paperboat server")
				}
			}
			if fresh && serverURL == "" {
				serverURL = strings.TrimSpace(buildinfo.DefaultServerURL)
			}
			if fresh && serverURL == "" {
				return localArgumentError("fresh pairing requires --server")
			}
			if fresh {
				token, _ := command.Flags().GetString("enrollment-token")
				tokenFile, _ := command.Flags().GetString("enrollment-token-file")
				if strings.TrimSpace(token) == "" && strings.TrimSpace(tokenFile) == "" {
					return localArgumentError("fresh pairing requires --enrollment-token or --enrollment-token-file")
				}
			}
			publicIdentityKey := base64.RawURLEncoding.EncodeToString(identityStore.Current().Public())
			if !fresh && registration.PublicIdentityKey != publicIdentityKey {
				return errors.New("machine setup identity does not match the current key; run `pb setup` to repair it")
			}
			if !fresh {
				serverURL = registration.ServerURL
			}
			arguments := []string{"bootstrap", "--server", serverURL}
			for _, name := range []string{"enrollment-token", "enrollment-token-file", "name", "shell", "state-root"} {
				value, err := command.Flags().GetString(name)
				if err != nil {
					return err
				}
				if strings.TrimSpace(value) != "" {
					arguments = append(arguments, "--"+name, value)
				}
			}
			operationOut, operationErr := command.OutOrStdout(), command.ErrOrStderr()
			if jsonOutput {
				operationOut, operationErr = io.Discard, io.Discard
			}
			code := hostruntimecmd.Execute(command.Context(), arguments, command.InOrStdin(), operationOut, operationErr)
			if code != 0 {
				if jsonOutput {
					return jsonFailureExitCodeError{code: code, message: "machine pairing failed"}
				}
				return exitCodeError{code: code}
			}
			if jsonOutput {
				paired, err := identityStore.Registration()
				if err != nil {
					return fmt.Errorf("read paired machine registration: %w", err)
				}
				return writeCLIJSON(command.OutOrStdout(), map[string]any{"machine_id": paired.MachineID, "environment_id": paired.EnvironmentID, "server_url": paired.ServerURL, "installation_generation": paired.InstallationGeneration})
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	command.Flags().String("enrollment-token", "", "single-use pairing token")
	command.Flags().String("enrollment-token-file", "", "absolute protected file containing a single-use pairing token")
	command.Flags().String("name", "", "machine name")
	command.Flags().String("shell", "", "absolute login shell")
	command.Flags().String("state-root", "", "runtime state directory")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func setupCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "setup",
		Short: "Set up this machine for Paperboat",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			operationOut, operationErr := command.OutOrStdout(), command.ErrOrStderr()
			if jsonOutput {
				operationOut, operationErr = io.Discard, io.Discard
			}
			var err error
			if runtime.GOOS == "windows" {
				sshPort, setupErr := setupPlatformHostPrerequisites(command.Context())
				if setupErr != nil {
					return fmt.Errorf("prepare Windows OpenSSH: %w", setupErr)
				}
				if sshPort != 0 {
					if err := command.Flags().Set("ssh-port", strconv.Itoa(int(sshPort))); err != nil {
						return err
					}
				}
			}
			sshPortValue := uint(0)
			sshPortValue, err = command.Flags().GetUint("ssh-port")
			if err != nil || sshPortValue == 0 || sshPortValue > 65535 {
				return invocationError(errors.New("--ssh-port must be between 1 and 65535"))
			}

			account, err := user.Current()
			if err != nil || strings.TrimSpace(account.Username) == "" {
				return commandPreparationFailure{step: prepareMachineUser, cause: err}
			}
			ctx := actionContext(command, nil)
			client, err := setupBackendClient(ctx)
			if err != nil {
				return err
			}
			stateRoot, err := command.Flags().GetString("state-root")
			if err != nil {
				return err
			}
			if stateRoot == "" {
				stateRoot = os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
			}
			if stateRoot == "" {
				stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
				if err != nil {
					return err
				}
			}
			identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
			if err != nil {
				return fmt.Errorf("open machine identity: %w", err)
			}
			workspaceRoot, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve workspace root: %w", err)
			}
			workspaceRoot = filepath.Clean(workspaceRoot)
			name, err := command.Flags().GetString("name")
			if err != nil {
				return err
			}
			if strings.TrimSpace(name) == "" {
				name, err = os.Hostname()
				if err != nil || strings.TrimSpace(name) == "" {
					return commandPreparationFailure{step: prepareMachineName, cause: err}
				}
			}
			key := identityStore.Current()
			publicIdentityKey := base64.RawURLEncoding.EncodeToString(key.Public())
			inboxPath, err := inbox.DefaultPath()
			if err != nil {
				return fmt.Errorf("resolve Paperboat Inbox: %w", err)
			}
			previousRegistration, previousRegistrationErr := identityStore.Registration()
			hadPreviousRegistration := previousRegistrationErr == nil
			if previousRegistrationErr != nil && !errors.Is(previousRegistrationErr, os.ErrNotExist) {
				return fmt.Errorf("load machine registration: %w", previousRegistrationErr)
			}
			if hadPreviousRegistration {
				inboxPath = previousRegistration.InboxPath
			}
			if err := inbox.EnsurePath(inboxPath); err != nil {
				return fmt.Errorf("prepare Paperboat Inbox: %w", err)
			}
			d, err := buildDeps(ctx)
			if err != nil {
				return err
			}
			if hadPreviousRegistration && (previousRegistration.ServerURL != d.cfg.ServerURL || previousRegistration.PublicIdentityKey != publicIdentityKey) {
				return errors.New("existing machine registration belongs to a different server or identity; setup cannot replace it")
			}
			machine, err := client.SetupMachine(command.Context(), api.MachineSetupInput{
				Alias: strings.TrimSpace(name), Platform: runtime.GOOS, Architecture: runtime.GOARCH,
				WorkspaceRoot: workspaceRoot, PublicIdentityKey: publicIdentityKey,
				RuntimeVersions: map[string]string{"pb": buildinfo.Version},
			})
			if err != nil {
				if errors.Is(err, api.ErrUnauthenticated) {
					return err
				}
				return setupFailure(err)
			}
			var priorRegistration *identity.Registration
			if hadPreviousRegistration {
				priorRegistration = &previousRegistration
			}
			if err := validateSetupMachineIdentity(machine, priorRegistration, publicIdentityKey); err != nil {
				return setupFailure(err)
			}
			registration := identity.Registration{
				ServerURL: d.cfg.ServerURL, MachineID: machine.ID, EnvironmentID: machine.EnvironmentID,
				PublicKeyID: key.ID, PublicIdentityKey: publicIdentityKey,
				InboxPath:              inboxPath,
				InstallationGeneration: machine.InstallationGeneration,
				UpdatedAt:              time.Now().UTC(),
			}
			if hadPreviousRegistration {
				registration.AccountID = previousRegistration.AccountID
				registration.SSHUser, registration.SSHPort = previousRegistration.SSHUser, previousRegistration.SSHPort
			}

			if err := identityStore.SaveRegistration(registration); err != nil {
				return setupFailure(fmt.Errorf("save machine registration: %w", err))
			}
			if err := saveSetupMachineControl(command.Context(), client, identityStore, machine, key.ID); err != nil {
				return setupFailure(err)
			}
			desiredCapabilities := machine.MachineCapabilities.Desired
			for flag, value := range map[string]*bool{
				"terminal":       &desiredCapabilities.Terminal,
				"managed-ssh":    &desiredCapabilities.ManagedSSH,
				"file-receive":   &desiredCapabilities.FileReceive,
				"preview-tunnel": &desiredCapabilities.PreviewTunnel,
			} {
				if command.Flags().Changed(flag) {
					*value, _ = command.Flags().GetBool(flag)
				}
			}
			if _, err := client.SetUserMachineCapabilities(command.Context(), machine.ID, newIdempotencyKey(), desiredCapabilities, machine.MachineCapabilities.DesiredVersion); err != nil {
				return setupFailure(fmt.Errorf("save incoming machine capabilities: %w", err))
			}
			if machine.Installation == nil {
				return setupFailure(errors.New("server did not return installation material"))
			}
			artifact := bootstrap.ArtifactTarget{
				Schema: machine.Installation.Artifact.Schema, Kind: machine.Installation.Artifact.Kind,
				Version: machine.Installation.Artifact.Version, Platform: machine.Installation.Artifact.Platform,
				Architecture: machine.Installation.Artifact.Architecture, RepositoryURL: machine.Installation.Artifact.RepositoryURL,
				TargetPath: machine.Installation.Artifact.TargetPath,
			}
			if err := verifySetupInstallSource(); err != nil {
				return setupFailure(fmt.Errorf("verify supplied installation executable: %w", err))
			}
			if _, err := client.RegisterManagedSSHTarget(command.Context(), machine.ID, uint64(machine.InstallationGeneration), account.Username, uint16(sshPortValue), newIdempotencyKey()); err != nil {
				return setupFailure(fmt.Errorf("register SSH target: %w", err))
			}
			registration.SSHUser, registration.SSHPort = account.Username, uint16(sshPortValue)
			if err := identityStore.SaveRegistration(registration); err != nil {
				return setupFailure(fmt.Errorf("save registered SSH target: %w", err))
			}
			resume, err := bootstrap.PrepareAuthenticatedSetupResume(stateRoot, d.cfg.ServerURL, publicIdentityKey, strings.TrimSpace(name), machine.ID, machine.InstallationGeneration, artifact, time.Now().UTC())
			if err != nil {
				if errors.Is(err, bootstrap.ErrResumeBinding) {
					return fmt.Errorf("machine setup cannot replace the protected unfinished installation: %w; run `pb bootstrap --state-root %s` to recover that installation before starting setup again", err, stateRoot)
				}
				return setupFailure(fmt.Errorf("prepare authenticated machine setup recovery: %w", err))
			}
			reusableIdentity, reusableIdentityErr := enrollment.LoadRuntimeIdentityForRenewal(stateRoot, time.Now().UTC())
			prepared, err := client.PrepareAuthenticatedMachineInstallation(command.Context(), machine.ID, resume.SetupOperationID, api.AuthenticatedMachineInstallationInput{
				Verifier: resume.Verifier, PublicIdentityKey: publicIdentityKey,
				InstallationGeneration: machine.InstallationGeneration, Artifact: machine.Installation.Artifact,
				SSHUser: account.Username, SSHPort: uint16(sshPortValue),
				CanReuseRuntimeIdentity: reusableIdentityErr == nil && reusableIdentity.MachineID == machine.ID && reusableIdentity.EnvironmentID == machine.ID,
			})
			if err != nil {
				return setupFailure(fmt.Errorf("prepare authenticated machine installation: %w", err))
			}
			resume.PairingStarted = true
			resume.PairingExpiresAt = prepared.ExpiresAt
			if err := bootstrap.SaveResume(stateRoot, resume); err != nil {
				return setupFailure(fmt.Errorf("persist authenticated machine installation: %w", err))
			}

			arguments := []string{"bootstrap", "--server", d.cfg.ServerURL, "--state-root", stateRoot, "--name", strings.TrimSpace(name)}
			if code := hostruntimecmd.Execute(command.Context(), arguments, command.InOrStdin(), operationOut, operationErr); code != 0 {
				return setupFailure(exitCodeError{code: code})
			}
			if err := rebindLocalDaemonService(command.Context(), d.cfg); err != nil {
				return setupFailure(fmt.Errorf("start machine local daemon: %w", err))
			}
			machine, err = doctorUserMachine(command.Context(), client, machine.ID)
			if err != nil {
				return setupFailure(fmt.Errorf("verify machine readiness: %w", err))
			}
			registration, err = identityStore.Registration()
			if err != nil {
				return setupFailure(fmt.Errorf("read completed machine registration: %w", err))
			}
			registration.InstallationGeneration, registration.UpdatedAt = machine.InstallationGeneration, time.Now().UTC()
			if err := identityStore.SaveRegistration(registration); err != nil {
				return setupFailure(fmt.Errorf("save machine registration: %w", err))
			}

			if err := exportSetupRecoveryKey(command); err != nil {
				return setupFailure(err)
			}
			if jsonOutput {
				recoveryOutput, _ := command.Flags().GetString("recovery-output")
				return writeCLIJSON(command.OutOrStdout(), map[string]any{"machine": machine, "inbox_path": inboxPath, "recovery_output": strings.TrimSpace(recoveryOutput)})
			}
			fmt.Fprintf(command.OutOrStdout(), "Set up %s (%s)\n", machine.Alias, machine.ID)
			return nil
		},
		SilenceUsage: true, SilenceErrors: true,
	}
	command.Flags().String("name", "", "machine name")
	command.Flags().String("state-root", "", "runtime state directory")
	command.Flags().Uint("ssh-port", 22, "existing loopback sshd port")
	command.Flags().Bool("terminal", true, "accept Paperboat terminal and exec")
	command.Flags().Bool("managed-ssh", true, "accept managed SSH tools")
	command.Flags().Bool("file-receive", true, "accept native Inbox transfers")
	command.Flags().Bool("preview-tunnel", true, "serve previews and tunnels")
	command.Flags().String("recovery-output", "", "new absolute file for the account recovery key")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

// setupFailure keeps the unified machine and its durable setup operation intact.
// Installation owns restoration of a previously working service; retry resumes pairing.
func setupFailure(cause error) error {
	return setupFailureError{cause: cause}
}

func saveSetupMachineControl(ctx context.Context, client *api.Client, identityStore *identity.Store, machine api.UserMachine, keyID string) error {
	operationID := newIdempotencyKey()
	controlBody, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
	}{operationID})
	if err != nil {
		return err
	}
	controlPath := "/v1/machines/" + machine.ID + "/control-credentials"
	proof, err := identityStore.MachineProof(operationID, http.MethodPost, controlPath, controlBody, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("prove machine identity: %w", err)
	}
	controlCredential, err := client.IssueMachineControlCredential(ctx, machine.ID, operationID, proof)
	if err != nil {
		return fmt.Errorf("issue machine control credential: %w", err)
	}
	if err := identityStore.SaveMachineControl(identity.MachineControl{
		MachineID: machine.ID, EnvironmentID: machine.EnvironmentID,
		InstallationGeneration: machine.InstallationGeneration, Credential: controlCredential.Credential,
		ExpiresAt: controlCredential.ExpiresAt, KeyID: keyID,
	}); err != nil {
		return fmt.Errorf("save machine control credential: %w", err)
	}
	return nil
}

func uninstallCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "uninstall",
		Short: "Completely remove Paperboat from this machine",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			hostname, err := os.Hostname()
			if err != nil || strings.TrimSpace(hostname) == "" {
				return commandPreparationFailure{step: prepareMachineName, cause: err}
			}
			first, _ := command.Flags().GetString("confirmation")
			second, _ := command.Flags().GetString("hostname")
			if !jsonOutput {
				reader := bufio.NewReader(command.InOrStdin())
				fmt.Fprintln(command.ErrOrStderr(), "This permanently removes Paperboat services, binaries, credentials, configuration, and runtime state. The Paperboat Inbox is preserved.")
				fmt.Fprint(command.ErrOrStderr(), "Type UNINSTALL PAPERBOAT to continue: ")
				first, err = reader.ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				fmt.Fprintf(command.ErrOrStderr(), "Type this machine hostname (%s) to confirm: ", hostname)
				second, err = reader.ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
			}
			if strings.TrimSpace(first) != "UNINSTALL PAPERBOAT" {
				return invocationError(errors.New("uninstall confirmation did not match"))
			}
			if strings.TrimSpace(second) != hostname {
				return invocationError(errors.New("hostname confirmation did not match"))
			}
			jsonWriter := command.OutOrStdout()
			if jsonOutput {
				command.SetOut(io.Discard)
				command.SetErr(io.Discard)
			}
			preservedInboxes, inboxErr := uninstallInboxPaths(command)
			executable, executableErr := os.Executable()
			home, homeErr := os.UserHomeDir()
			productHandoffReady := !platformProductHandoffRequired()
			daemonStopConfirmed := false
			cleanupErr := performUninstallCleanup([]uninstallCleanupStep{
				{name: "remove installed CLI manuals", run: func() error {
					if executableErr != nil {
						return executableErr
					}
					return removeInstalledManuals(executable)
				}},
				{name: "resolve Paperboat Inbox preservation", run: func() error { return inboxErr }},
				{name: "stop local daemon", run: func() error {
					if executableErr != nil {
						return executableErr
					}
					serviceCtx, cancelService := context.WithTimeout(context.WithoutCancel(command.Context()), 30*time.Second)
					defer cancelService()
					err := localdaemon.RemoveCurrentUserService(serviceCtx, executable)
					if err == nil {
						daemonStopConfirmed = true
					}
					return err
				}},
				{name: "remove managed OpenSSH configuration", run: func() error {
					if homeErr != nil {
						return homeErr
					}
					_, err := managedssh.UninstallOpenSSHConfig(home, uint32(os.Geteuid()))
					return err
				}},
				{name: "remove managed SSH public identity", run: func() error {
					if homeErr != nil {
						return homeErr
					}
					return managedssh.UninstallManagedIdentityPublicKey(home, uint32(os.Geteuid()))
				}},
				{name: "remove system Paperboat runtime", run: func() error {
					return purgePlatformRuntime(command)
				}},
				{name: "remove installed Paperboat product", run: func() error {
					if platformRequiresConfirmedDaemonStop() && !daemonStopConfirmed {
						return errors.New("local daemon ownership or termination was not confirmed; product removal was skipped")
					}
					if inboxErr != nil {
						return errors.New("Paperboat Inbox location could not be proven; product removal was skipped")
					}
					productCtx, cancelProduct := context.WithTimeout(context.WithoutCancel(command.Context()), 5*time.Minute)
					defer cancelProduct()
					err := removePlatformProductInstallation(productCtx, preservedInboxes, command.OutOrStdout())
					if err == nil {
						productHandoffReady = true
					}
					return err
				}},
				{name: "remove user Paperboat state", run: func() error {
					if inboxErr != nil {
						return errors.New("Paperboat Inbox location could not be proven; state removal was skipped")
					}
					if !productHandoffReady {
						return errors.New("Windows cleanup helper did not start; state removal was skipped so uninstall can be retried safely")
					}
					return purgeUserPaperboatState(command, preservedInboxes)
				}},
			})
			if cleanupErr != nil {
				if !jsonOutput {
					fmt.Fprintln(command.OutOrStdout(), "Paperboat attempted every local removal step. The Paperboat Inbox was preserved.")
				}
				return uninstallCleanupError{err: cleanupErr}
			}
			if jsonOutput {
				return writeCLIJSON(jsonWriter, map[string]any{"uninstalled": true, "hostname": hostname, "inbox_preserved": true})
			}
			fmt.Fprintln(command.OutOrStdout(), platformUninstallSuccessMessage())
			return nil
		},
		SilenceUsage: true, SilenceErrors: true,
	}
	command.Flags().String("state-root", "", "additional Paperboat runtime state directory to remove")
	command.Flags().String("confirmation", "", "exact confirmation phrase: UNINSTALL PAPERBOAT")
	command.Flags().String("hostname", "", "exact current hostname confirmation")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

type uninstallCleanupStep struct {
	name string
	run  func() error
}

// performUninstallCleanup deliberately runs every step. Complete uninstall is
// a local recovery boundary: a stopped service, unavailable broker, or failed
// network-shaped cleanup must never strand later-owned files or service state.
func performUninstallCleanup(steps []uninstallCleanupStep) error {
	var result error
	for _, step := range steps {
		if step.run == nil {
			result = errors.Join(result, uninstallStepFailure{name: step.name, err: errors.New("cleanup is unavailable")})
			continue
		}
		if err := step.run(); err != nil {
			result = errors.Join(result, uninstallStepFailure{name: step.name, err: err})
		}
	}
	return result
}

func purgeUserPaperboatState(command *cobra.Command, preserved []string) error {
	result := config.PurgeCredentialStore()
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".local", "bin", "pb"))
	} else {
		result = errors.Join(result, err)
	}
	configPath, _ := command.Flags().GetString("config")
	if configPath != "" {
		if cfg, err := config.Load(configPath); err == nil && filepath.IsAbs(cfg.Auth.ProfileDir) {
			paths = append(paths, cfg.Auth.ProfileDir)
		}
		paths = append(paths, configPath)
	} else if path, err := config.DefaultPath(); err == nil {
		if cfg, loadErr := config.Load(path); loadErr == nil && filepath.IsAbs(cfg.Auth.ProfileDir) {
			paths = append(paths, cfg.Auth.ProfileDir)
		}
		paths = append(paths, filepath.Dir(path))
	}
	if dir, err := config.DefaultCredentialDir(); err == nil {
		paths = append(paths, filepath.Dir(dir))
	}
	if path, err := userpaths.Cache("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if path, err := userpaths.Config("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if path, err := userpaths.State("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if path, err := userpaths.Data("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if root, err := helperconfig.DefaultStateRoot(os.Getenv); err == nil {
		paths = append(paths, root)
	}
	if root, err := command.Flags().GetString("state-root"); err == nil && root != "" {
		paths = append(paths, filepath.Clean(root))
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filesystemRoot(path) {
			result = errors.Join(result, errors.New("refusing unsafe Paperboat removal path"))
			continue
		}
		result = errors.Join(result, removePathPreserving(path, preserved))
	}
	return result
}

func uninstallInboxPaths(command *cobra.Command) ([]string, error) {
	roots := make(map[string]struct{})
	if root, err := helperconfig.DefaultStateRoot(os.Getenv); err == nil {
		roots[filepath.Clean(root)] = struct{}{}
	}
	if root, err := command.Flags().GetString("state-root"); err == nil && strings.TrimSpace(root) != "" {
		absolute, absoluteErr := filepath.Abs(root)
		if absoluteErr != nil {
			return nil, absoluteErr
		}
		roots[filepath.Clean(absolute)] = struct{}{}
	}
	paths := make(map[string]struct{})
	if path, err := inbox.DefaultPath(); err == nil {
		paths[filepath.Clean(path)] = struct{}{}
	}
	for root := range roots {
		registrationPath := filepath.Join(root, "machine-registration.json")
		info, err := os.Lstat(registrationPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 8192 {
			return nil, commandPreparationFailure{step: prepareInboxPreservation, cause: err}
		}
		encoded, err := os.ReadFile(registrationPath)
		if err != nil {
			return nil, err
		}
		var registration struct {
			InboxPath string `json:"inbox_path"`
		}
		if err := json.Unmarshal(encoded, &registration); err != nil || !filepath.IsAbs(registration.InboxPath) || filepath.Clean(registration.InboxPath) != registration.InboxPath || filesystemRoot(registration.InboxPath) {
			return nil, commandPreparationFailure{step: prepareInboxPreservation, cause: err}
		}
		paths[registration.InboxPath] = struct{}{}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func removePathPreserving(root string, preserved []string) error {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || filesystemRoot(root) {
		return errors.New("refusing unsafe Paperboat removal path")
	}
	for _, keep := range preserved {
		keep = filepath.Clean(keep)
		if pathWithinOrEqual(keep, root) {
			// The cleanup root is the Inbox or is nested inside it. Preserve the
			// complete Inbox tree, including user-created content unknown to us.
			return nil
		}
	}
	containsInbox := false
	for _, keep := range preserved {
		if pathWithinOrEqual(root, filepath.Clean(keep)) {
			containsInbox = true
			break
		}
	}
	if !containsInbox {
		return os.RemoveAll(root)
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	unsafeTraversal, traversalErr := unsafeCleanupPathTraversal(root, info)
	if traversalErr != nil {
		return traversalErr
	}
	if unsafeTraversal || !info.IsDir() {
		return errors.New("refusing to traverse an unsafe Paperboat cleanup path containing the Inbox")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		child := filepath.Join(root, entry.Name())
		result = errors.Join(result, removePathPreserving(child, preserved))
	}
	return result
}

func pathWithinOrEqual(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func filesystemRoot(path string) bool {
	path = filepath.Clean(path)
	return path == filepath.VolumeName(path)+string(os.PathSeparator)
}

type relayListResult struct {
	RelayID    string `json:"relay_id"`
	Name       string `json:"name"`
	Region     string `json:"region,omitempty"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	ObservedAt int64  `json:"observed_at,omitempty"`
}

func actionRelayList(command *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPathFlag(command))
	if err != nil {
		return err
	}
	if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(server)
		if err != nil {
			return err
		}
	}
	source, err := sessionauth.NewSource(cfg)
	if err != nil {
		return err
	}
	credential, err := source.Credential()
	if err != nil {
		return err
	}
	selfhost, pool, err := api.New(cfg.ServerURL, credential, nil).SelfhostInventory(command.Context(), "relay")
	if err != nil {
		return friendlyCommandError(err)
	}
	_, _, err = startLocalDaemonAndWait(command.Context(), cfg, installLocalDaemonService)
	if err != nil {
		return err
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return err
	}
	local, err := localapi.NewClient(paths.SocketPath, 45*time.Second)
	if err != nil {
		return err
	}
	inventory, err := local.RelayInventory(command.Context())
	if err != nil {
		return fmt.Errorf("verified native relay inventory: %w", err)
	}
	results := relayListResults(inventory, selfhost, pool.Mode, time.Now())
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(struct {
			Schema   string            `json:"schema"`
			PoolMode string            `json:"pool_mode"`
			Relays   []relayListResult `json:"relays"`
		}{Schema: "paperboat.relay-list/v1", PoolMode: pool.Mode, Relays: results})
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "RELAY\tNAME\tSOURCE\tREGION\tSTATUS")
	for _, result := range results {
		region := result.Region
		if region == "" {
			region = "-"
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", result.RelayID, result.Name, result.Source, region, result.Status)
	}
	return writer.Flush()
}

func relayListResults(inventory localapi.RelayInventory, selfhost []api.SelfhostInstallation, poolMode string, now time.Time) []relayListResult {
	metadata := make(map[string]api.SelfhostInstallation, len(selfhost))
	for _, installation := range selfhost {
		metadata[installation.NodeID] = installation
	}
	results := make([]relayListResult, 0, len(inventory.Candidates)+len(selfhost))
	seen := make(map[string]bool, len(inventory.Candidates))
	nowUnix := now.Unix()
	for _, candidate := range inventory.Candidates {
		if !slices.Contains(candidate.Roles, "relay") || !slices.Contains(candidate.Transports, "derp_quic") {
			continue
		}
		installation, isSelfhost := metadata[candidate.NodeID]
		if poolMode == "self-hosted-only" && (!isSelfhost || installation.ScopeKind == "global" || !installation.Selected) {
			continue
		}
		result := relayListResult{RelayID: candidate.NodeID, Name: candidate.NodeID, Region: candidate.Region, Source: "paperboat", Status: "unavailable", ObservedAt: candidate.ObservedAt}
		if candidate.State == "ready" && candidate.ObservedAt <= nowUnix && nowUnix-candidate.ObservedAt <= 15 && candidate.ExpiresAt > nowUnix {
			result.Status = "reported_ready"
		}
		if isSelfhost {
			result.Name = installation.Name
			if installation.ScopeKind != "global" {
				result.Source = "self-hosted"
			}
		}
		results = append(results, result)
		seen[candidate.NodeID] = true
	}
	for _, installation := range selfhost {
		if installation.Selected && installation.ScopeKind != "global" && !seen[installation.NodeID] {
			results = append(results, relayListResult{RelayID: installation.NodeID, Name: installation.Name, Source: "self-hosted", Status: "unavailable"})
		}
	}
	return results
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "pb [environment] [new]",
		Short: "Open Paperboat or connect to an environment terminal",
		Long: `Paperboat provides remote terminals, managed SSH and file transfers, previews,
tunnels, environment configuration, team management, and local runtime controls.

Run pb without arguments in a terminal to open the interactive home screen.
Run pb new to open a fresh terminal on this enrolled device in the current directory.
Use explicit subcommands in scripts; pb COMMAND --help describes each operation.
Use pb COMMAND --help --json for machine-readable command discovery.

The --json flag selects machine-readable output and suppresses interactive menus.
Raw terminal and native transfer commands reject unsupported JSON output before
execution. Streaming commands may emit multiple JSON records; consult their help.

Local preferences can define shortcuts, command defaults and TUI appearance.
Run pb config customize to edit them, or use --no-customization to bypass them.
Explicit flags override saved defaults. Built-in command names remain reserved.
Numeric invocations such as pb 3000 select the configured preview or tunnel action.

Run pb doctor for diagnostics. Check command output for partial changes and
recovery instructions before retrying an operation that may have created resources.`,
		Example: "  pb\n  pb new\n  pb auth login\n  pb environments\n  pb connect Studio\n  pb ssh Studio\n  pb preview 3000\n  pb tunnel create demo --port 3000\n  pb config customize\n  pb --no-customization environments --json",
		Args: commandArgs(func(command *cobra.Command, args []string) error {
			if command.ArgsLenAtDash() == 1 && len(args) >= 2 {
				return nil
			}
			return terminalArgs(0)(command, args)
		}),
		RunE: func(command *cobra.Command, args []string) error {
			if command.ArgsLenAtDash() == 1 {
				return actionExecCobra(command, args, true)
			}
			if len(args) == 0 {
				serverFlag := command.Flags().Lookup("server")
				if serverFlag != nil && serverFlag.Changed {
					cfg, err := config.Load(configPathFlag(command))
					if err != nil {
						return err
					}
					server, _ := command.Flags().GetString("server")
					normalized, err := config.NormalizeServerURL(server)
					if err != nil {
						return invocationError(err)
					}
					cfg.ServerURL = normalized
					if err := cfg.Save(); err != nil {
						return err
					}
					input, interactive := command.InOrStdin().(*os.File)
					if !interactive || !term.IsTerminal(int(input.Fd())) {
						if jsonOutputRequested(command) {
							return writeCLIJSON(command.OutOrStdout(), map[string]any{"server": normalized, "path": cfg.Path(), "updated": true})
						}
						return nil
					}
				}
				if jsonOutputRequested(command) {
					return writeCLIJSON(command.OutOrStdout(), map[string]any{"command": "pb", "version": buildinfo.Version, "commands": cliCommandCatalog(command.Root())})
				}
				return actionHome(command)
			}
			if jsonOutputRequested(command) {
				return unsupportedJSONOutputError{command: "pb terminal shorthand; use `pb exec --json` for machine output"}
			}
			if err := validateConnectInvocation(command); err != nil {
				return err
			}
			return actionConnect(actionContext(command, args))
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.Version = buildinfo.Version
	root.SetVersionTemplate(versionDisplay(buildinfo.Version))
	root.InitDefaultVersionFlag()
	root.Flags().Lookup("version").Shorthand = "v"
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return invocationError(err) })
	root.PersistentFlags().String("config", "", "path to the CLI config file")
	root.PersistentFlags().String("server", "", "paperboat-server base URL override")
	root.PersistentFlags().String("workspace", "", "resource workspace selector: personal or a team slug")
	root.PersistentFlags().Bool("json", false, "print machine-readable JSON")
	root.PersistentFlags().Bool("no-customization", false, "ignore local shortcuts, command defaults, and TUI preferences")
	root.SetHelpCommand(newCLIHelpCommand(root))
	root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if err := command.ValidateRequiredFlags(); err != nil {
			return invocationError(err)
		}
		if err := command.ValidateFlagGroups(); err != nil {
			// The registered audience flags are the only exclusive group. Build
			// public prose from their names, without exposing Cobra argument text.
			private := command.Flags().Lookup("private")
			team := command.Flags().Lookup("team")
			if private != nil && team != nil && private.Changed && team.Changed {
				return usageError{err: err, publicMessage: "Flags --team and --private are mutually exclusive. Choose one audience."}
			}
			return invocationError(err)
		}
		if err := captureWorkspaceInvocation(command, args); err != nil {
			return err
		}
		if err := prepareJSONCommand(command); err != nil {
			return err
		}
		showLocalUpdateNotice(command)
		return checkCLIUpdatePolicy(command)
	}
	addConnectFlags(root)

	connect := &cobra.Command{Use: "connect <environment> [new]", Short: "Create and attach to an environment terminal session", Args: commandArgs(terminalArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		if err := validateConnectInvocation(command); err != nil {
			return err
		}
		return actionConnect(actionContext(command, args))
	}}
	addConnectFlags(connect)
	root.AddCommand(connect)
	newTerminal := &cobra.Command{
		Use: "new", Short: "Open a fresh terminal on this device in the current directory",
		Long: "Create and attach a fresh durable terminal on this enrolled device in the current directory. The device runtime must be running and the selected workspace must authorize this device. Use --name to name the new session; use pb session attach to reconnect later.",
		Args: commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, args []string) error {
			if err := validateConnectInvocation(command); err != nil {
				return err
			}
			if command.Flags().Changed("session") {
				return invocationError(errors.New("pb new always creates a fresh session; use `pb session attach` to reconnect"))
			}
			machineID, cwd, err := localTerminalTarget()
			if err != nil {
				return err
			}
			return actionConnectTargetInDirectory(actionContext(command, nil), machineID, cwd)
		},
	}
	addConnectFlags(newTerminal)
	root.AddCommand(newTerminal)
	execCommand := &cobra.Command{Use: "exec <machine> [flags] -- <argv...>", Short: "Execute an exact command on a machine", Args: commandArgs(cobra.ArbitraryArgs), RunE: func(command *cobra.Command, args []string) error {
		return actionExecCobra(command, args, false)
	}}
	execCommand.Flags().String("cwd", "", "absolute remote working directory")
	execCommand.Flags().Duration("timeout", 0, "remote execution timeout")
	execCommand.Flags().Bool("pty", false, "allocate a remote PTY")
	execCommand.Flags().StringArray("env", nil, "remote environment name=value")
	execCommand.Flags().Bool("json", false, "emit paperboat.exec-event/v1 JSON Lines")
	root.AddCommand(execCommand)
	sshCommand := &cobra.Command{Use: "ssh [user@]<machine> [-- <OpenSSH arguments...>]", Short: "Connect to a machine with OpenSSH", Args: commandArgs(cobra.ArbitraryArgs), RunE: actionSSH}
	sshCommand.Flags().String("user", "", "remote operating-system user")
	sshTrustHost := &cobra.Command{Use: "trust-host <machine>", Short: "Approve a changed SSH host identity", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionSSHTrustHost}
	sshTrustHost.Flags().String("fingerprint", "", "exact pending SHA256 fingerprint")
	sshTrustHost.Flags().Bool("json", false, "print JSON")
	sshCommand.AddCommand(sshTrustHost)
	sshDoctor := &cobra.Command{Use: "doctor <machine>", Short: "Check SSH integration for a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionSSHDoctor}
	sshDoctor.Flags().Bool("json", false, "print JSON")
	sshCommand.AddCommand(sshDoctor)
	root.AddCommand(sshCommand)
	root.AddCommand(newManagedSSHToolCommand("scp"))
	root.AddCommand(newManagedSSHToolCommand("sftp"))
	root.AddCommand(newManagedSSHToolCommand("rsync"))
	sshProxyCommand := &cobra.Command{Use: "__ssh-proxy", Hidden: true, Args: commandArgs(cobra.NoArgs), RunE: actionSSHProxy}
	sshProxyCommand.Flags().String("host", "", "")
	sshProxyCommand.Flags().String("port", "", "")
	sshProxyCommand.Flags().String("user", "", "")
	root.AddCommand(sshProxyCommand)
	sshKnownHostsCommand := &cobra.Command{Use: "__ssh-known-hosts", Hidden: true, Args: commandArgs(cobra.NoArgs), RunE: actionSSHKnownHosts}
	sshKnownHostsCommand.Flags().String("host", "", "")
	sshKnownHostsCommand.Flags().String("port", "", "")
	root.AddCommand(sshKnownHostsCommand)

	environments := &cobra.Command{Use: "environments", Short: "List enrolled machines available to this account", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		jsonOutput, _ := command.Flags().GetBool("json")
		if !jsonOutput && term.IsTerminal(int(os.Stdin.Fd())) {
			return actionEnvironmentsList(command)
		}
		return actionRun(environmentsCommand().Action)(command, args)
	}}
	environments.Flags().Bool("json", false, "print JSON")
	inventoryFilterFlags(environments)
	root.AddCommand(environments)
	root.AddCommand(workspaceSwitchCommand())
	root.AddCommand(environmentVariablesCobraCommand())
	root.AddCommand(teamCobraCommand())
	root.AddCommand(desktopCommand())

	root.AddCommand(doctorCommandV1())
	ping := &cobra.Command{Use: "ping <machine>", Short: "Measure authenticated connectivity to a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionPing}
	ping.Flags().Int("count", 4, "number of authenticated native connections")
	ping.Flags().Duration("timeout", 10*time.Second, "timeout for each connection")
	ping.Flags().Bool("json", false, "print JSON")
	root.AddCommand(ping)
	relay := &cobra.Command{Use: "relay", Short: "Inspect hosted and self-hosted relays"}
	relayList := &cobra.Command{Use: "list", Short: "List hosted and self-hosted relays", Long: `List relay candidates from the signed native network authority and the
signed-in account's selected self-hosted relay pool. Mixed mode shows both
sources. Self-hosted-only mode shows only selected self-hosted relays,
including unavailable ones. Sign-in and the local daemon are required.

Reported_ready means a recent signed control-plane observation, not a live
data connection. Use --json for paperboat.relay-list/v1 output with the pool
mode, relays, and observation timestamps.`, Example: "  pb relay list\n  pb relay list --json", Args: commandArgs(cobra.NoArgs), RunE: actionRelayList}
	relayList.Flags().Bool("json", false, "print JSON")
	relay.AddCommand(relayList)
	root.AddCommand(relay)
	root.AddCommand(edgeCommand())

	authTree := specTree(authCommand(), "auth")
	authTree.AddCommand(workspaceSwitchCommand())
	loginAlias, _, _ := specTree(authCommand(), "auth").Find([]string{"login"})
	root.AddCommand(loginAlias)
	authTree.RunE = func(command *cobra.Command, _ []string) error {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return command.Help()
		}
		return actionHomeAccount(command)
	}
	root.AddCommand(authTree)
	logout := &cobra.Command{Use: "logout", Short: "Revoke and remove the active client session", Args: commandArgs(cobra.NoArgs), RunE: actionRun(authLogout)}
	logout.Flags().Bool("json", false, "print JSON")
	root.AddCommand(logout)
	configTree := specTree(configCommand(), "config")
	configTree.RunE = func(command *cobra.Command, _ []string) error {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return command.Help()
		}
		return actionHomeConfig(command)
	}
	configTree.AddCommand(statusBarConfigCommand(), localAccessConfigCommand(), configConflictCobraCommand(), configForceCobraCommand(), customizationCommand(), configRepositoryCobraCommand(), configSyncCobraCommand())
	root.AddCommand(configTree)
	root.AddCommand(previewCobraCommandV1())
	root.AddCommand(tunnelCobraCommandV1())
	access := &cobra.Command{Use: "access", Short: "Open authenticated private access", Args: commandArgs(cobra.NoArgs)}
	access.AddCommand(accessTunnelCobraCommandV1())
	access.AddCommand(accessMachineCobraCommandV1())
	root.AddCommand(access)
	root.AddCommand(specTree(inboxCommand(), "inbox"))
	root.AddCommand(sessionCobraCommand())
	root.AddCommand(userMachineCobraCommand())
	root.AddCommand(pairCommand())
	root.AddCommand(setupCommand())
	root.AddCommand(updateCommand())
	root.AddCommand(uninstallCommand())
	root.AddCommand(freshEnrollmentResetCommand())
	root.AddCommand(manualInstallCommand())
	root.AddCommand(sendCommand())
	root.AddCommand(statusCommand())
	root.AddCommand(waitCommand())
	root.AddCommand(bugreportCommand())
	root.AddCommand(daemoncmd.NewCommand())
	root.AddCommand(daemoncmd.ServiceCommand())
	root.AddCommand(daemoncmd.ResolveCommand())
	root.AddCommand(daemoncmd.TagCommand())
	root.AddCommand(daemoncmd.ApproveCommand())
	root.AddCommand(privilegedServiceOperationCommand())
	root.AddCommand(platformInstallCommand())
	root.AddCommand(platformUninstallHelperCommand())
	configureShellCompletion(root)
	root.InitDefaultCompletionCmd()
	enrichCommandHelp(root)
	return root
}

type updateResult struct {
	PreviousVersion   string `json:"previous_version"`
	Version           string `json:"version"`
	CLIUpdated        bool   `json:"cli_updated"`
	RuntimeUpdated    bool   `json:"runtime_updated"`
	ActivationPending bool   `json:"activation_pending,omitempty"`
}

func updateCommand() *cobra.Command {
	command := &cobra.Command{Use: "update", Short: "Update pb from the signed Paperboat release", Args: commandArgs(cobra.NoArgs), RunE: actionUpdate}
	command.Flags().Bool("json", false, "print JSON")
	check := &cobra.Command{Use: "check", Short: "Check the signed Paperboat release without installing it", Args: commandArgs(cobra.NoArgs), RunE: actionUpdateCheck}
	check.Flags().Bool("json", false, "print JSON")
	status := &cobra.Command{Use: "status", Short: "Show installed Paperboat update state", Args: commandArgs(cobra.NoArgs), RunE: actionUpdateStatus}
	status.Flags().Bool("json", false, "print JSON")
	command.Flags().String("approve", "", "install the exact downloaded candidate ID after reviewing it")
	download := &cobra.Command{Use: "download", Short: "Download and verify an update without installing it", Args: commandArgs(cobra.NoArgs), RunE: actionUpdateDownload}
	download.Flags().Bool("json", false, "print JSON")
	command.AddCommand(check, status, download, updateSettingsCommand())
	return command
}

type updateCheckResult struct {
	InstalledVersion string `json:"installed_version"`
	LatestVersion    string `json:"latest_version"`
	UpdateAvailable  bool   `json:"update_available"`
	Verified         bool   `json:"verified"`
}

var updateControlSocketForCommand = updatedControlSocket

const updateDaemonSnapshotTimeout = 2 * time.Second

// readLocalDaemonSnapshot is deliberately a direct read-only probe. Update
// status must report whether the authenticated daemon is actually present; it
// must not use localDaemonSnapshot, whose recovery path can install and start
// a daemon as a side effect.
var readLocalDaemonSnapshot = func(ctx context.Context) (localapi.Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, updateDaemonSnapshotTimeout)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, updateDaemonSnapshotTimeout)
	defer cancel()
	return client.Snapshot(probeCtx)
}

func actionUpdateCheck(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 30*time.Second)
	defer cancel()
	client, err := newUpdateControlClient(30 * time.Second)
	if err != nil {
		return err
	}
	response, err := client.Check(ctx)
	if err == nil {
		available, err := signedUpdateAvailable(buildinfo.Version, response.Version)
		if err != nil {
			return fmt.Errorf("validate signed update version: %w", err)
		}
		return writeUpdateCheckResult(command, response.Version, available)
	}
	return fmt.Errorf("check with paperboat-updated: %w", err)
}

func writeUpdateCheckResult(command *cobra.Command, latest string, available bool) error {
	result := updateCheckResult{InstalledVersion: buildinfo.Version, LatestVersion: latest, UpdateAvailable: available, Verified: true}
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": result})
	}
	if result.UpdateAvailable {
		fmt.Fprintf(command.OutOrStdout(), "Paperboat %s is available; installed version is %s.\n", result.LatestVersion, buildinfo.Version)
	} else {
		fmt.Fprintf(command.OutOrStdout(), "pb %s is up to date.\n", buildinfo.Version)
	}
	return nil
}

func signedUpdateAvailable(installed, latest string) (bool, error) {
	if latest == "" {
		return false, nil
	}
	comparison, err := selfupdate.CompareVersions(latest, installed)
	if err != nil {
		return false, err
	}
	return comparison > 0, nil
}

type updateStatusResult struct {
	OwnerMaintenance  *autoupdate.OwnerMaintenanceNotice `json:"owner_maintenance,omitempty"`
	Settings          *autoupdate.Preferences            `json:"settings,omitempty"`
	NextMaintenance   time.Time                          `json:"next_maintenance_at,omitempty"`
	LatestVersion     string                             `json:"latest_version,omitempty"`
	UpdateAvailable   bool                               `json:"update_available"`
	BlockedReason     string                             `json:"blocked_reason,omitempty"`
	RequiredVersion   string                             `json:"required_version,omitempty"`
	CLIVersion        string                             `json:"cli_version"`
	RuntimeVersion    string                             `json:"runtime_version"`
	RuntimeAvailable  bool                               `json:"runtime_available"`
	RuntimeState      string                             `json:"runtime_state,omitempty"`
	ActivationPending bool                               `json:"activation_pending"`
	ActivationFailure string                             `json:"activation_failure,omitempty"`
	LastCheck         time.Time                          `json:"last_check,omitempty"`
	NextCheck         time.Time                          `json:"next_check,omitempty"`
	LastFailure       string                             `json:"last_failure,omitempty"`
	Candidate         *workerupdate.PreparedCandidate    `json:"candidate,omitempty"`
}

func actionUpdateStatus(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 10*time.Second)
	defer cancel()
	client, err := newUpdateControlClient(10 * time.Second)
	if err != nil {
		return err
	}
	response, err := client.Status(ctx)
	if err == nil {
		var snapshot *localapi.Snapshot
		if daemonSnapshot, snapshotErr := readLocalDaemonSnapshot(ctx); snapshotErr == nil {
			snapshot = &daemonSnapshot
		}
		result := updateStatusCommandResult(buildinfo.Version, response, snapshot)
		return writeUpdateStatusResult(command, result)
	}
	return fmt.Errorf("read paperboat-updated status: %w", err)
}

func updateStatusCommandResult(cliVersion string, response updated.ControlResponse, snapshot *localapi.Snapshot) updateStatusResult {
	result := updateStatusResult{BlockedReason: response.Observation.BlockedReason, RequiredVersion: response.Observation.RequiredVersion, CLIVersion: cliVersion, ActivationPending: response.Pending, ActivationFailure: response.ActivationFailure, LastCheck: response.Observation.CheckedAt, NextCheck: response.Observation.NextCheckAt, LastFailure: response.Observation.Failure, Candidate: response.Candidate}
	result.LatestVersion = response.Observation.Version
	// A prepared candidate is verified release evidence even when the last
	// periodic observation predates its download. Preserve a newer observation.
	if response.Candidate != nil {
		candidateVersion := response.Candidate.Version
		observedVersion := result.LatestVersion
		if observedVersion == "" {
			observedVersion = candidateVersion
		}
		if comparison, compareErr := selfupdate.CompareVersions(candidateVersion, observedVersion); compareErr == nil && comparison >= 0 {
			result.LatestVersion = candidateVersion
		}
	}
	result.Settings, result.NextMaintenance, result.OwnerMaintenance = response.Settings, response.NextMaintenanceAt, response.OwnerMaintenance
	installed := response.Transaction.ActiveVersion
	if installed == "" {
		installed = cliVersion
	}
	if available, err := signedUpdateAvailable(installed, result.LatestVersion); err == nil {
		result.UpdateAvailable = available
	}
	if snapshot != nil {
		result.RuntimeState = snapshot.DaemonState
		if daemonSnapshotAvailable(*snapshot) {
			result.RuntimeVersion = snapshot.DaemonVersion
			result.RuntimeAvailable = true
		}
	}
	return result
}

func daemonSnapshotAvailable(snapshot localapi.Snapshot) bool {
	return snapshot.DaemonVersion != "" && (snapshot.DaemonState == "ready" || snapshot.DaemonState == "degraded")
}

func writeUpdateStatusResult(command *cobra.Command, result updateStatusResult) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": result})
	}
	fmt.Fprintf(command.OutOrStdout(), "CLI: %s\n", result.CLIVersion)
	writeOwnerMaintenanceNotice(command.OutOrStdout(), result.OwnerMaintenance)
	if result.Settings != nil {
		if result.Settings.Enabled {
			fmt.Fprintf(command.OutOrStdout(), "Automatic updates: on, at %s machine local time\n", result.Settings.LocalTime)
		} else {
			fmt.Fprintln(command.OutOrStdout(), "Automatic updates: off")
		}
	}
	if !result.NextMaintenance.IsZero() {
		fmt.Fprintf(command.OutOrStdout(), "Next maintenance: %s\n", result.NextMaintenance.Local().Format(time.RFC3339))
	}
	if result.RuntimeAvailable {
		fmt.Fprintf(command.OutOrStdout(), "Runtime: %s\n", result.RuntimeVersion)
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Runtime: unavailable")
	}
	if result.RuntimeState != "" {
		fmt.Fprintf(command.OutOrStdout(), "Runtime state: %s\n", result.RuntimeState)
	}
	if result.UpdateAvailable && result.Candidate == nil {
		fmt.Fprintf(command.OutOrStdout(), "Paperboat %s is available. Run `pb update` to download and review it.\n", result.LatestVersion)
	}
	if result.Candidate != nil {
		writePreparedUpdate(command, result.Candidate)
	} else if result.ActivationPending {
		fmt.Fprintln(command.OutOrStdout(), "Activation: pending")
	} else if result.ActivationFailure != "" {
		fmt.Fprintf(command.OutOrStdout(), "Activation: failed (%s)\n", result.ActivationFailure)
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Activation: complete")
	}
	if !result.NextCheck.IsZero() {
		fmt.Fprintf(command.OutOrStdout(), "Next automatic check: %s\n", result.NextCheck.Local().Format(time.RFC3339))
	}
	if result.LastFailure != "" {
		fmt.Fprintf(command.OutOrStdout(), "Last update failure: %s\n", result.LastFailure)
	}

	return nil
}

func actionUpdateDownload(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
	defer cancel()
	client, err := newUpdateControlClient(15 * time.Minute)
	if err != nil {
		return err
	}
	response, err := updateWithProgress(command, ctx, client.Download)
	if err != nil {
		return fmt.Errorf("download signed update: %w", err)
	}
	return writeDownloadedUpdate(command, response)
}

func writePreparedUpdate(command *cobra.Command, candidate *workerupdate.PreparedCandidate) {
	fmt.Fprintf(command.OutOrStdout(), "Downloaded Paperboat %s (%s/%s, %d bytes).\nSHA256: %s\n", candidate.Version, candidate.Platform, candidate.Architecture, candidate.Length, candidate.SHA256)
	fmt.Fprintf(command.OutOrStdout(), "Review this update, then install with:\n  pb update --approve %s\n", candidate.ID)
	if candidate.OwnerMaintenance {
		fmt.Fprintln(command.OutOrStdout(), "This update replaces the process owner. Approving it ends running terminals and commands; connections reconnect afterward.")
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Installation briefly interrupts active connections. Running shells and commands stay alive while clients reconnect.")
	}
}

func writeDownloadedUpdate(command *cobra.Command, response updated.ControlResponse) error {
	if updateJSON(command) {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": response})
	}
	if response.Candidate != nil {
		writePreparedUpdate(command, response.Candidate)
	} else {
		fmt.Fprintln(command.OutOrStdout(), "Paperboat is up to date.")
	}
	return nil
}

var updateInputIsTerminal = func(command *cobra.Command) bool {
	input, ok := command.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(input.Fd()))
}

func actionUpdate(command *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Minute)
	defer cancel()
	client, err := newUpdateControlClient(15 * time.Minute)
	if err != nil {
		return err
	}
	approvalID, _ := command.Flags().GetString("approve")
	if approvalID == "" {
		response, err := updateWithProgress(command, ctx, client.Download)
		if err != nil {
			return fmt.Errorf("download signed update: %w", err)
		}
		if response.Candidate == nil || updateJSON(command) || !updateInputIsTerminal(command) {
			return writeDownloadedUpdate(command, response)
		}
		writePreparedUpdate(command, response.Candidate)
		fmt.Fprintf(command.OutOrStdout(), "Install Paperboat %s now? [y/N] ", response.Candidate.Version)
		answer, err := bufio.NewReader(io.LimitReader(command.InOrStdin(), 128)).ReadString('\n')
		if err != nil {
			return fmt.Errorf("read update approval: %w", err)
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(command.OutOrStdout(), "Update remains downloaded; installation was not requested.")
			return nil
		}
		approvalID = response.Candidate.ID
	}
	response, err := updateWithProgress(command, ctx, func(ctx context.Context) (updated.ControlResponse, error) { return client.Install(ctx, approvalID) })
	if err != nil {
		return fmt.Errorf("install approved update: %w", err)
	}
	var snapshot *localapi.Snapshot
	if response.Updated && !response.Pending {
		if value, err := readLocalDaemonSnapshot(ctx); err == nil {
			snapshot = &value
		}
	}
	return writeUpdateResult(command, updateCommandResult(buildinfo.Version, response, snapshot), response.Version)
}

func updateCommandResult(previousVersion string, response updated.ControlResponse, snapshot *localapi.Snapshot) updateResult {
	completed := response.Updated && !response.Pending
	runtimeUpdated := completed && snapshot != nil && daemonSnapshotAvailable(*snapshot) && snapshot.DaemonVersion == response.Version
	return updateResult{PreviousVersion: previousVersion, Version: response.Version, CLIUpdated: completed, RuntimeUpdated: runtimeUpdated, ActivationPending: response.Pending}
}

var updateProgressInterval = 5 * time.Second

// updateWithProgress keeps the control protocol single-response and bounded,
// while making the deliberately long health-monitoring hold visible to human
// users. JSON mode remains machine-clean: progress is omitted entirely.
func updateWithProgress(command *cobra.Command, ctx context.Context, update func(context.Context) (updated.ControlResponse, error)) (updated.ControlResponse, error) {
	if update == nil {
		return updated.ControlResponse{}, errors.New("update operation is unavailable")
	}
	if updateJSON(command) {
		return update(ctx)
	}
	fmt.Fprintln(command.ErrOrStderr(), "Processing the signed Paperboat update...")
	type result struct {
		response updated.ControlResponse
		err      error
	}
	resultCh := make(chan result, 1)
	started := time.Now()
	go func() {
		response, err := update(ctx)
		resultCh <- result{response: response, err: err}
	}()
	interval := updateProgressInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case result := <-resultCh:
			return result.response, result.err
		case <-ctx.Done():
			finished := <-resultCh
			return finished.response, errors.Join(finished.err, ctx.Err())
		case <-ticker.C:
			fmt.Fprintf(command.ErrOrStderr(), "Update is still in progress (%s)...\n", time.Since(started).Round(time.Second))
		}
	}
}

func updateJSON(command *cobra.Command) bool {
	jsonOutput, _ := command.Flags().GetBool("json")
	return jsonOutput
}

func writeUpdateResult(command *cobra.Command, result updateResult, version string) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": result})
	}
	if result.ActivationPending {
		fmt.Fprintf(command.OutOrStdout(), "Paperboat %s is staged. Activation is in progress; run `pb update status` to confirm completion.\n", version)
		return nil
	}
	if !result.CLIUpdated && !result.RuntimeUpdated {
		fmt.Fprintf(command.OutOrStdout(), "pb %s is already up to date.\n", version)
		return nil
	}
	if result.CLIUpdated && !result.RuntimeUpdated {
		fmt.Fprintf(command.OutOrStdout(), "Updated pb to %s.\n", version)
		return nil
	}
	fmt.Fprintf(command.OutOrStdout(), "Updated Paperboat runtime to %s.\n", version)
	return nil
}

func newUpdateControlClient(timeout time.Duration) (*updated.Client, error) {
	socket, err := updateControlSocketForCommand()
	if err != nil {
		return nil, fmt.Errorf("resolve the current user's updater: %w", err)
	}
	return updated.NewClient(socket, timeout)
}

func updatedControlSocket() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	var layout service.Layout
	if runtime.GOOS == "windows" {
		layout, err = service.WindowsUserLayout(account.Uid)
	} else {
		uid, parseErr := strconv.Atoi(account.Uid)
		if parseErr != nil {
			return "", parseErr
		}
		layout, err = service.UserLayout(runtime.GOOS, uid)
	}
	return layout.UpdaterSocket, err
}

const shellCompletionDeadline = 200 * time.Millisecond

func shellCompletionContext(command *cobra.Command) context.Context {
	if command != nil && command.Context() != nil {
		return command.Context()
	}
	return context.Background()
}

func configureShellCompletion(root *cobra.Command) {
	if root == nil {
		return
	}
	machine := machineCompletion
	for _, path := range [][]string{{"connect"}, {"exec"}, {"ssh"}, {"ping"}, {"doctor"}, {"wait"}, {"machine", "revoke"}} {
		if command, _, err := root.Find(path); err == nil && command != nil {
			command.ValidArgsFunction = machine
		}
	}
	for _, path := range [][]string{{"session", "list"}} {
		if command, _, err := root.Find(path); err == nil && command != nil {
			command.ValidArgsFunction = sessionCompletion
		}
	}
	if command, _, err := root.Find([]string{"session", "attach"}); err == nil && command != nil {
		command.ValidArgsFunction = attachSessionCompletion
	}
	for _, parent := range []string{"session"} {
		for _, child := range []string{"rename", "close", "delete"} {
			if command, _, err := root.Find([]string{parent, child}); err == nil && command != nil {
				command.ValidArgsFunction = sessionCompletion
			}
		}
	}
	if command, _, err := root.Find([]string{"preview", "stop"}); err == nil && command != nil {
		command.ValidArgsFunction = previewStopCompletion
	}
	if command, _, err := root.Find([]string{"send"}); err == nil && command != nil {
		_ = command.RegisterFlagCompletionFunc("to", transferTargetCompletion)
		_ = command.RegisterFlagCompletionFunc("session", allSessionCompletion)
	}
	if command, _, err := root.Find([]string{"send", "destination", "set"}); err == nil && command != nil {
		command.ValidArgsFunction = transferTargetCompletion
	}
	root.ValidArgsFunction = personalizedMachineCompletion
}

func machineCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return localCompletion(command, "machine", "", toComplete)
}

func transferTargetCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return localCompletion(command, "transfer_target", "", toComplete)
}

func sessionCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return machineCompletion(command, nil, toComplete)
	}
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sessionsForEnvironmentCompletion(command, args[0], toComplete)
}

func allSessionCompletion(command *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return localCompletion(command, "session", "", toComplete)
}

func attachSessionCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if len(args) == 1 {
		return sessionsForEnvironmentCompletion(command, args[0], toComplete)
	}
	return localCompletionKinds(command, map[string]bool{"machine": true, "session": true}, "", toComplete)
}

func sessionsForEnvironmentCompletion(command *cobra.Command, environment, toComplete string) ([]string, cobra.ShellCompDirective) {
	return localCompletion(command, "session", environment, toComplete)
}

func localCompletion(command *cobra.Command, kind, environment, prefix string) ([]string, cobra.ShellCompDirective) {
	return localCompletionKinds(command, map[string]bool{kind: true}, environment, prefix)
}

func localCompletionKinds(command *cobra.Command, kinds map[string]bool, environment, prefix string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(shellCompletionContext(command), shellCompletionDeadline)
	defer cancel()
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := localapi.NewClient(paths.SocketPath, shellCompletionDeadline)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	snapshot, err := client.Completions(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	environmentID := ""
	if environment != "" {
		for _, item := range snapshot.Items {
			if item.Kind == "machine" && strings.EqualFold(item.Value, environment) {
				environmentID = item.EnvironmentID
				break
			}
		}
		if environmentID == "" {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
	values := make([]string, 0)
	seen := make(map[string]bool)
	for _, item := range snapshot.Items {
		if !kinds[item.Kind] || environmentID != "" && item.EnvironmentID != environmentID || seen[item.Value] || !strings.HasPrefix(strings.ToLower(item.Value), strings.ToLower(prefix)) {
			continue
		}
		seen[item.Value] = true
		values = append(values, item.Value+"\t"+item.Description)
	}
	sort.Strings(values)
	return values, cobra.ShellCompDirectiveNoFileComp
}

func sessionCompletionValues(items []api.TerminalSession, prefix string) []string {
	values := make([]string, 0, len(items)*2)
	seen := make(map[string]bool)
	for _, item := range items {
		for _, value := range []string{item.Name, item.ID} {
			if value == "" || seen[value] || !strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
				continue
			}
			seen[value] = true
			values = append(values, value+"\t"+item.State)
		}
	}
	sort.Strings(values)
	return values
}

func actionHome(command *cobra.Command) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return commandRejection{reason: commandRejectNoninteractiveHome}
	}
	previousContext := command.Context()
	homeContext, cancelHome := context.WithCancel(previousContext)
	command.SetContext(homeContext)
	defer func() { cancelHome(); command.SetContext(previousContext) }()
	endScreen := selector.BeginScreen(command.ErrOrStderr())
	defer endScreen()
	if prefetch, prefetchErr := startHomePrefetch(command); prefetchErr == nil {
		command.SetContext(context.WithValue(command.Context(), homePrefetchContextKey{}, prefetch))
	}
	primeHomeFileIndex()
	revealEmail := false
	for {
		emailAction := "ctrl+e show email"
		if revealEmail {
			emailAction = "ctrl+e hide email"
		}
		selection, err := selector.ChooseWithAction(selector.Options{
			Header:  homeBrand(command, revealEmail),
			Title:   "What do you want to do?",
			Stdin:   os.Stdin,
			Context: command.Context(), Output: command.ErrOrStderr(),
			Footer:        "↑/↓ move  enter/click select  " + emailAction + "  esc exit",
			Actions:       map[string]string{"ctrl+e": "toggle-email"},
			HeaderActions: map[int]string{2: "toggle-email"},
			Items:         personalizedHomeItems(preferences.FromContext(command.Context())),
		})
		if err != nil {
			if errors.Is(err, selector.ErrCanceled) || errors.Is(err, selector.ErrInterrupted) {
				return nil
			}
			return err
		}
		if selection.Action == "toggle-email" {
			revealEmail = !revealEmail
			continue
		}
		err = runHomeAction(command, selection.Item.ID)
		if errors.Is(err, selector.ErrInterrupted) {
			return nil
		}
		if interactiveCanceled(err) || err == nil {
			continue
		}
		if displayErr := showHomeFailure(command, err); displayErr != nil {
			return displayErr
		}
	}
}

const homePrefetchFreshness = 5 * time.Second

type homePrefetchContextKey struct{}

type asyncHomeValue[T any] struct {
	done      chan struct{}
	value     T
	err       error
	fetchedAt time.Time
}

func startAsyncHomeValue[T any](ctx context.Context, load func(context.Context) (T, error)) *asyncHomeValue[T] {
	value := &asyncHomeValue[T]{done: make(chan struct{})}
	go func() {
		defer close(value.done)
		value.value, value.err = load(ctx)
		value.fetchedAt = time.Now()
	}()
	return value
}

func (v *asyncHomeValue[T]) ready() bool {
	select {
	case <-v.done:
		return true
	default:
		return false
	}
}

func (v *asyncHomeValue[T]) await(ctx context.Context) (T, error) {
	select {
	case <-v.done:
		return v.value, v.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (v *asyncHomeValue[T]) fresh() bool {
	return v.ready() && v.err == nil && time.Since(v.fetchedAt) <= homePrefetchFreshness
}

type homePrefetch struct {
	favorites *asyncHomeValue[favoriteSet]
	machines  *asyncHomeValue[[]api.UserMachine]
	sessions  *asyncHomeValue[[]machineSession]

	machinesClaimed atomic.Bool
	sessionsClaimed atomic.Bool
}

func startHomePrefetch(command *cobra.Command) (*homePrefetch, error) {
	client, err := backendForCommand(command)
	if err != nil {
		return nil, err
	}
	ctx := command.Context()
	prefetch := &homePrefetch{
		favorites: startAsyncHomeValue(ctx, func(ctx context.Context) (favoriteSet, error) { return loadFavorites(ctx, client) }),
		machines:  startAsyncHomeValue(ctx, client.ListUserMachines),
	}
	prefetch.sessions = startAsyncHomeValue(ctx, func(ctx context.Context) ([]machineSession, error) {
		machines, loadErr := prefetch.machines.await(ctx)
		if loadErr != nil {
			return nil, loadErr
		}
		return listMachineSessionsForMachines(ctx, client, machines)
	})
	return prefetch, nil
}

func homePrefetchFor(command *cobra.Command) *homePrefetch {
	value, _ := command.Context().Value(homePrefetchContextKey{}).(*homePrefetch)
	return value
}

func primeHomeFileIndex() {
	go func() {
		root, rootErr := os.UserHomeDir()
		cachePath, cacheErr := fileindex.CachePath()
		if rootErr == nil && cacheErr == nil {
			fileindex.RefreshInBackground(root, cachePath)
		}
	}()
}

func runHomeAction(command *cobra.Command, action string) error {
	if strings.HasPrefix(action, "shortcut:") {
		return runPreferenceFavorite(command, strings.TrimPrefix(action, "shortcut:"))
	}
	switch action {
	case "customize":
		return editPreferences(command)
	case "previews":
		return actionHomePreviews(command)
	case "commands":
		return actionHomeCommands(command, nil)
	case "team":
		return actionHomeTeams(command)
	case "send":
		return actionHomeSentFiles(command)
	case "sessions":
		return actionHomeSessions(command)
	case "environment-variables":
		return runEnvironmentVariablesTUI(command)
	case "machines":
		return actionHomeMachines(command)
	case "inbox":
		return actionHomeInbox(command)
	case "config":
		return actionHomeConfig(command)
	case "doctor":
		return actionHomeDoctor(command)
	case "account":
		return actionHomeAccount(command)
	case "switch-workspace":
		selected, err := selectWorkspace(command, nil)
		if err != nil {
			return err
		}
		if !selected {
			return selector.ErrCanceled
		}
		return selector.ErrInterrupted
	default:
		return errors.New("unknown Paperboat action")
	}
}

func actionHomeInbox(command *cobra.Command) error {
	ctx := actionContext(command, nil)
	client, err := backendClient(ctx)
	if err != nil {
		return err
	}
	me, err := client.Me(ctx.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	for {
		requests, listErr := client.PendingTeamInboxRequests(ctx.Context)
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		pending := slices.DeleteFunc(requests, func(request api.TeamInboxRequest) bool {
			return request.RecipientAccount != me.ID || request.Status != "pending"
		})
		if len(pending) == 0 {
			return showInformation(command, "Team Inbox", "No file requests are waiting for your approval.", nil)
		}
		items := make([]selector.Item, len(pending))
		for i, request := range pending {
			items[i] = selector.Item{ID: request.RequestID, Title: fmt.Sprintf("%d file(s) from %s", len(request.Files), request.SenderAccount), Description: "Expires " + relativeTimestamp(request.ExpiresAt)}
		}
		selection, selectErr := selector.ChooseWithAction(selector.Options{Title: "Team Inbox approvals", Subtitle: "Approval applies only to the listed names, sizes, hashes, recipient and transfer", Items: items, Footer: "enter review  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if selectErr != nil {
			return selectErr
		}
		request := pending[slices.IndexFunc(pending, func(request api.TeamInboxRequest) bool { return request.RequestID == selection.Item.ID })]
		lines := make([]string, len(request.Files))
		for i, file := range request.Files {
			lines[i] = fmt.Sprintf("%s — %d bytes — SHA-256 %s", file.Basename, file.Size, file.SHA256)
		}
		action, actionErr := chooseHomeAction(command, "File request from "+request.SenderAccount, []selector.Item{{ID: "approve", Title: "Approve exact files", Description: strings.Join(lines, "; ")}, {ID: "decline", Title: "Decline", Description: "No file bytes will be accepted"}})
		if actionErr != nil {
			if errors.Is(actionErr, selector.ErrCanceled) {
				continue
			}
			return actionErr
		}
		if _, err = client.DecideTeamInboxRequest(ctx.Context, request.RequestID, action.ID, request.DecisionGeneration); err != nil {
			return friendlyCommandError(err)
		}
	}
}

func versionDisplay(version string) string {
	return brandDisplay(version, "") + "\n"
}

func homeBrand(command *cobra.Command, revealEmail bool) string {
	account := "Not signed in"
	configPath, _ := command.Flags().GetString("config")
	cfg, err := config.Load(configPath)
	if err == nil {
		if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
			cfg.ServerURL, err = config.NormalizeServerURL(server)
		}
	}
	if err == nil && cfg.ServerURL != "" {
		if store, storeErr := config.ProfileStoreFor(cfg); storeErr == nil {
			if profile, profileErr := store.Load(cfg.ServerURL); profileErr == nil {
				account = firstNonEmpty(profile.Account.Email, profile.Account.DisplayName, profile.Account.ID, account)
			}
		}
	}
	if !revealEmail {
		account = maskEmail(account)
	}
	if workspace, workspaceErr := effectiveWorkspace(command); workspaceErr == nil && workspace != "" {
		account += " · " + workspaceDisplayName(workspace)
	}
	return brandDisplay(buildinfo.Version, account)
}

func workspaceDisplayName(selector string) string {
	if selector == "personal" {
		return "Personal workspace"
	}
	return selector + " workspace"
}

func maskEmail(value string) string {
	local, domain, ok := strings.Cut(value, "@")
	if !ok {
		return value
	}
	maskPart := func(part string) string {
		return strings.Repeat("█", len([]rune(part)))
	}
	domainParts := strings.Split(domain, ".")
	for index, part := range domainParts {
		domainParts[index] = maskPart(part)
	}
	return maskPart(local) + "@" + strings.Join(domainParts, ".")
}

func brandDisplay(version, account string) string {
	art := []string{"      ▄█▄", "  ▄▄▝▀▀▀▀▀▘▄▄", "   ▀███████▀"}
	details := []string{"Paperboat", "Version " + version, account}
	artWidth := 0
	for _, line := range art {
		artWidth = max(artWidth, ansi.StringWidth(line))
	}
	lines := make([]string, len(art))
	for index, line := range art {
		lines[index] = line
		if details[index] != "" {
			lines[index] += strings.Repeat(" ", artWidth-ansi.StringWidth(line)+3) + details[index]
		}
	}
	return strings.Join(lines, "\n")
}

func actionEnvironmentsList(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	machines, err := client.ListUserMachinesFiltered(command.Context(), inventoryFilters(command))
	if err != nil {
		return friendlyCommandError(err)
	}
	currentMachineID, _ := configuredMachineID()
	sortMachinesForDisplay(machines, nil, currentMachineID)
	items := make([]selector.Item, 0, len(machines))
	for _, machine := range machines {
		items = append(items, selector.Item{ID: machine.ID, Title: machineDisplayTitle(machine, currentMachineID), Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.ID + " " + machine.WorkspaceRoot + " " + machineStatusSearch(machine)})
	}
	_, err = selector.Choose(selector.Options{Title: "Machines", Subtitle: "Computers available to this account", Items: items, Empty: "No machines yet", Footer: "↑/↓ inspect  type to filter  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return err
}

func actionHomeOwnedSessions(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	usePrefetch := true
	for {
		var favorites favoriteSet
		var sessions []machineSession
		var listErr error
		loaded := false
		if usePrefetch {
			usePrefetch = false
			if prefetch := homePrefetchFor(command); prefetch != nil && prefetch.sessionsClaimed.CompareAndSwap(false, true) {
				work := func(ctx context.Context) error {
					var loadErr error
					favorites, loadErr = prefetch.favorites.await(ctx)
					if loadErr == nil {
						sessions, loadErr = prefetch.sessions.await(ctx)
					}
					return loadErr
				}
				listErr = runPrefetchedHomeLoad(command, "Terminal sessions", "Loading sessions", prefetch.favorites.ready() && prefetch.sessions.ready(), work)
				loaded = listErr == nil && prefetch.favorites.fresh() && prefetch.sessions.fresh()
			}
		}
		if !loaded {
			listErr = homeLoading(command, "Terminal sessions", "Loading sessions", func(ctx context.Context) error {
				var err error
				favorites, err = loadFavorites(ctx, client)
				if err != nil {
					return err
				}
				sessions, err = listMachineSessions(ctx, client)
				return err
			})
		}
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		selected, selectErr := selectMachineSession(command, sessions, favorites)
		if errors.Is(selectErr, errFavoriteToggle) {
			if favoriteErr := setFavorite(command.Context(), client, "session", machineSessionFavoriteID(selected), !favorites.IsFavorite("session", machineSessionFavoriteID(selected))); favoriteErr != nil {
				return favoriteErr
			}
			continue
		}
		if selectErr != nil {
			return selectErr
		}
		if err := actionHomeSessionActions(command, selected.target, selected.session); err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

type machineSession struct {
	target  environmentTarget
	session api.TerminalSession
}

func listMachineSessions(ctx context.Context, client *api.Client) ([]machineSession, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return nil, err
	}
	return listMachineSessionsForMachines(ctx, client, machines)
}

func listMachineSessionsForMachines(ctx context.Context, client *api.Client, machines []api.UserMachine) ([]machineSession, error) {
	machines = slices.Clone(machines)
	machines = slices.DeleteFunc(machines, func(machine api.UserMachine) bool {
		return !machine.Capabilities.TerminalHost.Configured
	})
	results := make(chan []machineSession, len(machines))
	errorsOut := make(chan error, len(machines))
	workers := make(chan struct{}, 4)
	var group sync.WaitGroup
	for _, machine := range machines {
		machine := machine
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				errorsOut <- ctx.Err()
				return
			}
			items, loadErr := client.ListUserMachineTerminalSessions(ctx, machine.ID)
			if loadErr != nil {
				errorsOut <- loadErr
				return
			}
			target := environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}
			mapped := make([]machineSession, 0, len(items))
			for _, session := range items {
				mapped = append(mapped, machineSession{target: target, session: session})
			}
			results <- mapped
		}()
	}
	group.Wait()
	close(results)
	close(errorsOut)
	var loadErrors []error
	for loadErr := range errorsOut {
		loadErrors = append(loadErrors, loadErr)
	}
	if loadErr := errors.Join(loadErrors...); loadErr != nil {
		return nil, loadErr
	}
	var sessions []machineSession
	for result := range results {
		sessions = append(sessions, result...)
	}
	slices.SortStableFunc(sessions, func(a, b machineSession) int {
		return b.activity().Compare(a.activity())
	})
	return sessions, nil
}

func (s machineSession) activity() time.Time {
	if s.session.LastActiveAt != nil {
		return *s.session.LastActiveAt
	}
	if !s.session.UpdatedAt.IsZero() {
		return s.session.UpdatedAt
	}
	return s.session.CreatedAt
}

func selectMachineSession(command *cobra.Command, sessions []machineSession, favorites favoriteSet) (machineSession, error) {
	slices.SortStableFunc(sessions, func(a, b machineSession) int {
		return compareFavorites(favorites.IsFavorite("session", machineSessionFavoriteID(a)), favorites.IsFavorite("session", machineSessionFavoriteID(b)))
	})
	items := make([]selector.Item, 0, len(sessions))
	byID := make(map[string]machineSession, len(sessions))
	for _, entry := range sessions {
		item := machineSessionIdentificationItem(command.Context(), entry, favorites)
		items = append(items, item)
		byID[item.ID] = entry
	}
	client, clientErr := backendForCommand(command)
	if clientErr != nil {
		return machineSession{}, clientErr
	}
	refresh := func(ctx context.Context) ([]selector.Item, error) {
		fresh, err := refreshSessionIdentification(ctx, client, sessions)
		if err != nil {
			return nil, err
		}
		updated := make([]selector.Item, 0, len(fresh))
		for _, entry := range fresh {
			key := entry.target.id + ":" + entry.session.ID
			if _, ok := byID[key]; !ok {
				continue
			}
			updated = append(updated, machineSessionIdentificationItem(command.Context(), entry, favorites))
		}
		return updated, nil
	}
	selected, err := selector.ChooseWithAction(selector.Options{Context: command.Context(), Refresh: refresh, Title: "Terminal sessions", Subtitle: "All machine sessions, newest first", Items: items, Empty: "no terminal sessions are available", Footer: "↑/↓ move  enter open  ctrl+f favorite  esc back", Actions: map[string]string{"ctrl+f": "favorite"}, Stdin: os.Stdin, Output: os.Stderr})
	entry := byID[selected.Item.ID]
	if err == nil && selected.Action == "favorite" {
		err = errFavoriteToggle
	}
	return entry, err
}

func machineSessionFavoriteID(entry machineSession) string {
	return entry.target.id + ":" + entry.session.ID
}

func actionHomeMachines(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	usePrefetch := true
	for {
		var favorites favoriteSet
		var machines []api.UserMachine
		var err error
		loaded := false
		if usePrefetch {
			usePrefetch = false
			if prefetch := homePrefetchFor(command); prefetch != nil && prefetch.machinesClaimed.CompareAndSwap(false, true) {
				work := func(ctx context.Context) error {
					var loadErr error
					favorites, loadErr = prefetch.favorites.await(ctx)
					if loadErr == nil {
						machines, loadErr = prefetch.machines.await(ctx)
					}
					return loadErr
				}
				err = runPrefetchedHomeLoad(command, "Machines", "Loading machines", prefetch.favorites.ready() && prefetch.machines.ready(), work)
				loaded = err == nil && prefetch.favorites.fresh() && prefetch.machines.fresh()
			}
		}
		if !loaded {
			err = homeLoading(command, "Machines", "Loading machines", func(ctx context.Context) error {
				var loadErr error
				favorites, loadErr = loadFavorites(ctx, client)
				if loadErr != nil {
					return loadErr
				}
				machines, loadErr = client.ListUserMachines(ctx)
				return loadErr
			})
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		currentMachineID, _ := configuredMachineID()
		sortMachinesForDisplay(machines, favorites, currentMachineID)
		items := make([]selector.Item, 0, len(machines)+1)
		byID := make(map[string]api.UserMachine, len(machines))
		for _, machine := range machines {
			if machine.ID == currentMachineID {
				continue
			}
			favorite := favorites.IsFavorite("machine", machine.ID)
			items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.WorkspaceRoot + " " + machineStatusSearch(machine) + " favorite starred", Favorite: favorite})
			byID[machine.ID] = machine
		}
		for _, machine := range machines {
			if machine.ID != currentMachineID {
				continue
			}
			favorite := favorites.IsFavorite("machine", machine.ID)
			items = append(items, selector.Item{ID: machine.ID, Title: machineDisplayTitle(machine, currentMachineID), Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.WorkspaceRoot + " " + machineStatusSearch(machine) + " this machine favorite starred", Favorite: favorite})
			byID[machine.ID] = machine
		}
		items = append(items, selector.Item{ID: "add", Title: "+ Add machine", Description: "Set up another computer", Search: "new pair enroll", Action: true})
		selection, selectErr := selector.ChooseWithAction(selector.Options{Title: "Machines", Subtitle: "Paired computers", Items: items, Footer: "↑/↓ move  enter open  ctrl+f favorite  esc back", Actions: map[string]string{"ctrl+f": "favorite"}, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if selectErr != nil {
			return selectErr
		}
		choice := selection.Item
		if choice.ID == "add" && selection.Action == "favorite" {
			continue
		}
		if choice.ID == "add" {
			if addErr := executeInteractiveCommand(command, []string{"machine", "add"}); addErr != nil {
				if interactiveCanceled(addErr) {
					continue
				}
				return addErr
			}
			continue
		}
		machine := byID[choice.ID]
		if selection.Action == "favorite" {
			if favoriteErr := setFavorite(command.Context(), client, "machine", machine.ID, !favorites.IsFavorite("machine", machine.ID)); favoriteErr != nil {
				return favoriteErr
			}
			continue
		}
		for {
			action, actionErr := chooseMachineHomeAction(command, machine)
			if errors.Is(actionErr, selector.ErrCanceled) {
				break
			}
			if actionErr != nil {
				return actionErr
			}
			var runErr error
			switch action.ID {
			case "terminal":
				runErr = executeInteractiveCommand(command, []string{"connect", machine.ID, "new"})
			case "sessions":
				runErr = actionHomeMachineSessions(command, client, machine)
				if interactiveCanceled(runErr) {
					runErr = nil
				}
			case "environment-variables":
				runErr = runEnvironmentVariableScopeTUI(command, client, environmentVariableTarget{machineID: machine.ID, machineName: machine.Alias})
			case "send":
				runErr = actionHomeSendToMachine(command, machine)
			case "rename":
				name, readErr := prompt.Text(prompt.TextOptions{Title: "Rename machine", Description: machine.Alias, Initial: machine.Alias, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: machinename.Validate})
				if readErr != nil {
					runErr = readErr
					break
				}
				runErr = runHomeResult(command, []string{"machine", "rename", machine.ID, name})
				if runErr == nil {
					machine.Alias = name
				}
			case "allow-sleep":
				runErr = runHomeResult(command, []string{"machine", "availability", machine.ID, "--mode", "allow-sleep"})
			case "keep-awake":
				runErr = runHomeResult(command, []string{"machine", "availability", machine.ID, "--mode", "keep-awake"})
			default:
				runErr = errors.New("unknown machine action")
			}
			if interactiveCanceled(runErr) {
				continue
			}
			if runErr != nil {
				if errors.Is(runErr, selector.ErrInterrupted) {
					return runErr
				}
				if err := showHomeFailure(command, runErr); err != nil {
					return err
				}
			}
		}
	}
}

func machineHomeActions(machine api.UserMachine) []selector.Item {
	actions := make([]selector.Item, 0, 8)
	actions = append(actions, selector.Item{ID: "rename", Title: "Rename", Description: "Change this machine's alias"})
	if machineSupportsEnvironmentInjection(machine) {
		actions = append(actions, selector.Item{ID: "environment-variables", Title: "ENV Injection", Description: "Manage variables applied to new processes on this machine"})
	}
	if machine.Capabilities.TerminalHost.Configured {
		actions = append(actions,
			selector.Item{ID: "terminal", Title: "Create terminal session", Description: "Start and attach to a new durable session"},
		)
	}
	if machine.Capabilities.FileReceive.Configured {
		if sourceMachineID, err := configuredMachineID(); err != nil || sourceMachineID != machine.ID {
			actions = append(actions, selector.Item{ID: "send", Title: "Send files", Description: "Search, select, drop, or paste files for this machine"})
		}
	}
	if machine.Capabilities.TerminalHost.Configured {
		actions = append(actions, selector.Item{ID: "sessions", Title: "Sessions", Description: "List durable terminal sessions on this machine"})
	}
	actions = append(actions,
		selector.Item{ID: "allow-sleep", Title: "Allow sleep", Description: "Let normal operating-system sleep policy apply"},
		selector.Item{ID: "keep-awake", Title: "Keep awake", Description: "Request availability even when idle"},
	)
	return actions
}

func chooseMachineHomeAction(command *cobra.Command, machine api.UserMachine) (selector.Item, error) {
	return selector.Choose(selector.Options{Title: machine.Alias, Subtitle: machineStatusSummary(machine), Items: machineHomeActions(machine), Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
}

func machineStatusSummary(machine api.UserMachine) string {
	return machineAvailabilityLabel(machine) + "  ·  " + machineCapabilityLabel(machine)
}

func machineStatusSearch(machine api.UserMachine) string {
	return strings.Join([]string{machineAvailabilityLabel(machine), machineCapabilityLabel(machine)}, " ")
}

func machineAvailabilityLabel(machine api.UserMachine) string {
	if machine.Online {
		return "Online"
	}
	switch strings.ToLower(strings.TrimSpace(machine.State)) {
	case "revoked", "deleted":
		return "Unavailable"
	default:
		return "Offline"
	}
}

func machineCapabilityLabel(machine api.UserMachine) string {
	count := 0
	for _, enabled := range []bool{machine.MachineCapabilities.Desired.Terminal, machine.MachineCapabilities.Desired.ManagedSSH, machine.MachineCapabilities.Desired.FileReceive, machine.MachineCapabilities.Desired.PreviewTunnel} {
		if enabled {
			count++
		}
	}
	return fmt.Sprintf("%d incoming services", count)
}

func machineDisplayTitle(machine api.UserMachine, currentMachineID string) string {
	if machine.ID == currentMachineID {
		return machine.Alias + " (this machine)"
	}
	return machine.Alias
}

func sortMachinesForDisplay(machines []api.UserMachine, favorites favoriteSet, currentMachineID string) {
	slices.SortStableFunc(machines, func(a, b api.UserMachine) int {
		aCurrent, bCurrent := a.ID == currentMachineID, b.ID == currentMachineID
		if aCurrent != bCurrent {
			if aCurrent {
				return 1
			}
			return -1
		}
		return compareFavorites(favorites.IsFavorite("machine", a.ID), favorites.IsFavorite("machine", b.ID))
	})
}

func actionHomeSendToMachine(command *cobra.Command, machine api.UserMachine) error {
	var candidates []string
	searchRoot, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cachePath, err := fileindex.CachePath()
	if err != nil {
		return err
	}
	loadIndex := func(ctx context.Context) error {
		var refreshErr error
		candidates, refreshErr = fileindex.Current(ctx, searchRoot, cachePath)
		return refreshErr
	}
	// A previously written index is sufficient to populate the picker
	// immediately. Refresh it in the background so opening Send Files never
	// blocks on a full profile walk (notably Windows' large AppData tree).
	if cached, cacheOK := fileindex.Load(searchRoot, cachePath); cacheOK {
		candidates = cached
		fileindex.RefreshInBackground(searchRoot, cachePath)
	} else if fileindex.RefreshReady(cachePath) {
		err = loadIndex(command.Context())
	} else {
		err = homeLoading(command, "Send files", "Refreshing files", loadIndex)
	}
	if err != nil {
		return err
	}
	defer fileindex.RefreshInBackground(searchRoot, cachePath)
	items := make([]selector.Item, 0, len(candidates)+1)
	for _, path := range candidates {
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		display := path
		if relative, relativeErr := filepath.Rel(searchRoot, path); relativeErr == nil {
			display = filepath.Join("~", relative)
		}
		items = append(items, selector.Item{ID: path, Title: display, Description: "File"})
	}
	selection, err := selector.ChooseWithAction(selector.Options{Title: "Send files to " + machine.Alias, Subtitle: "Type to fuzzy-search files in your home folder", Items: items, Empty: "Start typing to search", RequireFilter: true, Footer: "type to search  enter/click send  ctrl+p paste or drop paths  esc back", Actions: map[string]string{"ctrl+p": "paths"}, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	if err != nil {
		return err
	}
	if selection.Action != "paths" {
		return executeInteractiveCommand(command, []string{"send", selection.Item.ID, "--to", machine.ID})
	}
	value, err := prompt.Text(prompt.TextOptions{Title: "Send files to " + machine.Alias, Description: "Drag files here or paste one or more file or folder paths", Placeholder: "/path/to/file", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
		_, parseErr := droppedFilePaths(value)
		return parseErr
	}})
	if errors.Is(err, prompt.ErrCanceled) {
		return selector.ErrCanceled
	}
	if err != nil {
		return err
	}
	paths, err := droppedFilePaths(value)
	if err != nil {
		return err
	}
	arguments := append([]string{"send"}, paths...)
	arguments = append(arguments, "--to", machine.ID)
	return executeInteractiveCommand(command, arguments)
}

func droppedFilePaths(value string) ([]string, error) {
	paths, err := splitDroppedPaths(strings.TrimSpace(value))
	if err != nil || len(paths) == 0 {
		return nil, errors.New("drop or paste at least one valid file or folder path")
	}
	for index, path := range paths {
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) {
			absolute, absoluteErr := filepath.Abs(path)
			if absoluteErr != nil {
				return nil, absoluteErr
			}
			path = absolute
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, commandPreparationFailure{step: prepareSelectedFile, cause: statErr}
		}
		paths[index] = path
	}
	return paths, nil
}

func homeLoading(command *cobra.Command, title, detail string, work func(context.Context) error) error {
	return selector.Loading(command.Context(), title, detail, os.Stdin, command.ErrOrStderr(), work)
}

func runPrefetchedHomeLoad(command *cobra.Command, title, detail string, ready bool, work func(context.Context) error) error {
	if ready {
		return work(command.Context())
	}
	return homeLoading(command, title, detail, work)
}

func actionHomeMachineSessions(command *cobra.Command, client *api.Client, machine api.UserMachine) error {
	target := environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}
	for {
		records, err := listTerminalSessionsForTarget(command.Context(), client, target)
		if err != nil {
			return friendlyCommandError(err)
		}
		favorites, err := loadFavorites(command.Context(), client)
		if err != nil {
			return err
		}
		sessions := make([]machineSession, 0, len(records))
		for _, session := range records {
			sessions = append(sessions, machineSession{target: target, session: session})
		}
		selected, err := selectMachineSession(command, sessions, favorites)
		if errors.Is(err, errFavoriteToggle) {
			id := machineSessionFavoriteID(selected)
			if err := setFavorite(command.Context(), client, "session", id, !favorites.IsFavorite("session", id)); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := actionHomeSessionActions(command, target, selected.session); err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

func actionHomeAccount(command *cobra.Command) error {
	for {
		ctx := actionContext(command, nil)
		d, err := buildDeps(ctx)
		if err != nil {
			return err
		}
		items := []selector.Item{{ID: "status", Title: "Account status", Description: "Not signed in"}, {ID: "login", Title: "Sign in", Description: "Approve this CLI in your browser"}}
		if _, credentialErr := d.auth.Credential(); credentialErr == nil {
			items = []selector.Item{
				{ID: "status", Title: "Account status", Description: "Signed in"},
				{ID: "change-account", Title: "Switch account", Description: "Sign in with another account through browser approval"},
				{ID: "logout", Title: "Sign out", Description: "Revoke this CLI session"},
			}
		}
		choice, err := chooseHomeAction(command, "Account", items)
		if err != nil {
			return err
		}
		switch choice.ID {
		case "status":
			client, clientErr := backendForCommand(command)
			if clientErr != nil {
				if infoErr := showInformation(command, "Account", "Not signed in", nil); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
					return infoErr
				}
				continue
			}
			var me api.Me
			meErr := homeLoading(command, "Account", "Loading account", func(ctx context.Context) error {
				var loadErr error
				me, loadErr = client.Me(ctx)
				return loadErr
			})
			if meErr != nil {
				return friendlyCommandError(meErr)
			}
			if infoErr := showInformation(command, "Account", firstNonEmpty(me.Email, me.DisplayName, me.ID), []selector.Item{{ID: "server", Title: "Paperboat server", Description: d.cfg.ServerURL}}); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
				return infoErr
			}
		case "login":
			if err := executeInteractiveCommand(command, []string{"auth", "login"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
		case "change-account":
			if err := executeInteractiveCommand(command, []string{"auth", "login", "--change-account"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
			return selector.ErrInterrupted
		case "logout":
			if err := executeInteractiveCommand(command, []string{"auth", "logout"}); err != nil {
				if interactiveCanceled(err) {
					continue
				}
				return err
			}
		}
	}
}

func actionHomeConfig(command *cobra.Command) error {
	for {
		cfg, loadErr := config.Load(configPathFlag(command))
		if loadErr != nil {
			return loadErr
		}
		choice, err := chooseHomeAction(command, "Configuration", []selector.Item{
			{ID: "customize", Title: "Customize your CLI", Description: "Shortcuts, defaults, themes, keys, and home layout"},
			{ID: "server", Title: "Paperboat server", Description: orNone(cfg.ServerURL), Search: "edit url endpoint"},
			{ID: "auth", Title: "File credential fallback", Description: onOff(cfg.Auth.AllowFileFallback), Search: "toggle auth credentials"},
			{ID: "status-bar", Title: "Status bar", Description: cfg.StatusBar.Mode + "  ·  " + cfg.StatusBar.Theme + "  ·  fullscreen " + cfg.StatusBar.Fullscreen},
			{ID: "sync", Title: "Config sync", Description: "Enable, configure, inspect or disable sync on this machine"},
			{ID: "path", Title: "Configuration file", Description: cfg.Path()},
		})
		if err != nil {
			return err
		}
		switch choice.ID {
		case "customize":
			if err := editPreferences(command); err != nil && !interactiveCanceled(err) {
				return err
			}
		case "server":
			value, inputErr := prompt.Text(prompt.TextOptions{Title: "Paperboat server", Description: "Control-plane URL used by this CLI", Placeholder: "https://api.pprbt.dev", Initial: cfg.ServerURL, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
				if strings.TrimSpace(value) == "" {
					return nil
				}
				_, normalizeErr := config.NormalizeServerURL(value)
				return normalizeErr
			}})
			if errors.Is(inputErr, prompt.ErrCanceled) {
				continue
			}
			if inputErr != nil {
				return inputErr
			}
			if strings.TrimSpace(value) == "" {
				cfg.ServerURL = ""
			} else {
				cfg.ServerURL, loadErr = config.NormalizeServerURL(value)
				if loadErr != nil {
					return loadErr
				}
			}
			if saveErr := cfg.Save(); saveErr != nil {
				return saveErr
			}
		case "auth":
			cfg.Auth.AllowFileFallback = !cfg.Auth.AllowFileFallback
			if saveErr := cfg.Save(); saveErr != nil {
				return saveErr
			}
		case "status-bar":
			if statusErr := actionHomeStatusBar(command); !errors.Is(statusErr, selector.ErrCanceled) {
				return statusErr
			}
		case "sync":
			if syncErr := actionHomeConfigSync(command); syncErr != nil && !interactiveCanceled(syncErr) {
				if err := showHomeFailure(command, syncErr); err != nil && !interactiveCanceled(err) {
					return err
				}
			}
		case "path":
			if infoErr := showInformation(command, "Configuration file", cfg.Path(), nil); infoErr != nil && !errors.Is(infoErr, selector.ErrCanceled) {
				return infoErr
			}
		}
	}
}

func actionHomeStatusBar(command *cobra.Command) error {
	for {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		choice, err := chooseHomeAction(command, "Status bar", []selector.Item{
			{ID: "mode", Title: "Mode", Description: cfg.StatusBar.Mode},
			{ID: "fullscreen", Title: "Full-screen applications", Description: cfg.StatusBar.Fullscreen},
			{ID: "theme", Title: "Theme", Description: cfg.StatusBar.Theme},
			{ID: "privacy", Title: "Privacy", Description: onOff(cfg.StatusBar.Privacy)},
			{ID: "title", Title: "Terminal title", Description: onOff(cfg.StatusBar.TerminalTitle)},
			{ID: "reset", Title: "Restore defaults", Description: "Reset all status-bar preferences"},
		})
		if err != nil {
			return err
		}
		switch choice.ID {
		case "mode":
			cfg.StatusBar.Mode, err = chooseValue(command, "Status bar mode", cfg.StatusBar.Mode, []string{"auto", "on", "off"})
		case "fullscreen":
			cfg.StatusBar.Fullscreen, err = chooseValue(command, "Full-screen applications", cfg.StatusBar.Fullscreen, []string{"hide", "show"})
		case "theme":
			cfg.StatusBar.Theme, err = chooseValue(command, "Status bar theme", cfg.StatusBar.Theme, []string{"terminal", "dark", "light", "mono"})
		case "privacy":
			cfg.StatusBar.Privacy = !cfg.StatusBar.Privacy
		case "title":
			cfg.StatusBar.TerminalTitle = !cfg.StatusBar.TerminalTitle
		case "reset":
			cfg.StatusBar = config.DefaultStatusBarConfig()
		}
		if errors.Is(err, selector.ErrCanceled) {
			continue
		}
		if err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
	}
}

func chooseValue(command *cobra.Command, title, current string, values []string) (string, error) {
	items := make([]selector.Item, 0, len(values))
	for _, value := range values {
		description := ""
		if value == current {
			description = "Current"
		}
		items = append(items, selector.Item{ID: value, Title: value, Description: description})
	}
	choice, err := selector.Choose(selector.Options{Title: title, Subtitle: "Choose a value", Items: items, Initial: current, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return choice.ID, err
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func actionHomeDoctor(command *cobra.Command) error {
	for {
		items, err := homeDoctorItems(command)
		if err != nil {
			return err
		}
		choice, err := selector.Choose(selector.Options{Title: "Diagnostics", Subtitle: "Local setup and Paperboat connectivity", Items: items, Footer: "enter view details · esc back", Context: command.Context(), Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if choice.ID != "config" {
			if err := showHomeText(command, choice.Title, homeDoctorDetail(choice)); err != nil {
				return err
			}
			continue
		}
		if err = actionHomeConfigSync(command); err != nil && !interactiveCanceled(err) {
			if err = showHomeFailure(command, err); err != nil && !interactiveCanceled(err) {
				return err
			}
		}
	}
}

func homeDoctorItems(command *cobra.Command) ([]selector.Item, error) {
	report := collectLocalDoctor(command.Context())
	items := []selector.Item{
		{ID: "setup", Title: "Local setup", Description: report.SetupState},
		{ID: "identity", Title: "Machine identity", Description: report.IdentityState},
		{ID: "credential", Title: "Machine credential", Description: report.CredentialState},
		{ID: "inbox", Title: "Paperboat Inbox", Description: report.InboxState + "  ·  " + report.InboxPath},
		{ID: "config", Title: "Config sync", Description: diagnosticConfigSync(report.ConfigService, nil, errors.New("assignment not checked")), Action: true},
		{ID: "runtime", Title: "Host runtime", Description: report.HostRuntime},
		{ID: "workloads", Title: "Local workloads", Description: diagnosticWorkloads(report)},
	}
	ctx := actionContext(command, nil)
	d, err := buildDeps(ctx)
	if err == nil {
		credential, credentialErr := d.auth.Credential()
		if credentialErr != nil {
			items = append(items, selector.Item{ID: "auth", Title: "Account", Description: diagnosticCredentialFailure(credentialErr)})
		} else if strings.TrimSpace(d.cfg.ServerURL) == "" {
			items = append(items, selector.Item{ID: "backend", Title: "Control plane", Description: "server not configured"})
		} else {
			var me api.Me
			meErr := homeLoading(command, "Diagnostics", "Checking control plane", func(ctx context.Context) error {
				var loadErr error
				me, loadErr = api.New(d.cfg.ServerURL, credential, nil).Me(ctx)
				return loadErr
			})
			if meErr != nil {
				items = append(items, selector.Item{ID: "backend", Title: "Control plane", Description: "unavailable  ·  " + userFacingError(meErr)})
			} else {
				items = append(items, selector.Item{ID: "auth", Title: "Account", Description: firstNonEmpty(me.Email, me.DisplayName, me.ID)}, selector.Item{ID: "backend", Title: "Control plane", Description: "authenticated"})
				if report.MachineID != "" {
					checkCtx, cancel := context.WithTimeout(command.Context(), 5*time.Second)
					assignment, assignmentErr := api.New(d.cfg.ServerURL, credential, nil).ConfigAssignment(checkCtx, report.MachineID)
					cancel()
					for index := range items {
						if items[index].ID == "config" {
							items[index].Description = diagnosticConfigSync(report.ConfigService, &assignment, assignmentErr)
						}
					}
					if assignmentErr == nil && configAssignmentEnabled(assignment) && report.ConfigService == "not_installed" {
						report.RecoveryActions = append(report.RecoveryActions, "configuration sync is assigned but its local worker is missing; reapply the configuration assignment to reinstall it")
					}
				}
			}
		}
	}
	if err != nil {
		items = append(items, selector.Item{ID: "auth", Title: "Account", Description: "saved sign-in could not be inspected"})
	}
	for index, recovery := range report.RecoveryActions {
		items = append(items, selector.Item{ID: fmt.Sprintf("recovery-%d", index), Title: "Needs attention", Description: recovery})
	}
	return items, nil
}

func showInformation(command *cobra.Command, title, subtitle string, items []selector.Item) error {
	for {
		choice, err := selector.Choose(selector.Options{Title: title, Subtitle: subtitle, Items: items, Empty: "Press Esc to go back", Footer: "enter view details  type to filter  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if err := showHomeText(command, choice.Title, choice.Description); err != nil && !interactiveCanceled(err) {
			return err
		}
	}
}

func chooseHomeAction(command *cobra.Command, title string, items []selector.Item) (selector.Item, error) {
	choice, err := selector.Choose(selector.Options{Title: title, Subtitle: "Choose an action", Items: items, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return choice, err
}

func compareFavorites(a, b bool) int {
	if a == b {
		return 0
	}
	if a {
		return -1
	}
	return 1
}

type favoriteSet map[string]struct{}

func (f favoriteSet) IsFavorite(kind, id string) bool {
	_, ok := f[kind+":"+id]
	return ok
}

func (f favoriteSet) Set(kind, id string, favorite bool) {
	key := kind + ":" + id
	if favorite {
		f[key] = struct{}{}
		return
	}
	delete(f, key)
}

func loadFavorites(ctx context.Context, client *api.Client) (favoriteSet, error) {
	items, err := client.ListFavorites(ctx)
	if err != nil {
		return nil, friendlyCommandError(err)
	}
	favorites := make(favoriteSet, len(items))
	for _, item := range items {
		favorites[item.Kind+":"+item.ResourceID] = struct{}{}
	}
	return favorites, nil
}

var errFavoriteToggle = errors.New("favorite toggle requested")

func setFavorite(ctx context.Context, client *api.Client, kind, id string, favorite bool) error {
	_, err := client.SetFavorite(ctx, kind, id, favorite)
	if err == nil {
		return nil
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "favorite_limit_reached" {
		return errors.New("you can favorite up to five items; unfavorite another machine, session, or preview first")
	}
	return friendlyCommandError(err)
}

func backendForCommand(command *cobra.Command) (*api.Client, error) {
	ctx := actionContext(command, nil)
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, err
	}
	credential, err := d.auth.Credential()
	if err != nil {
		return nil, err
	}
	return newWorkspaceAPIClient(ctx, d.cfg.ServerURL, credential)
}

func executeInteractiveCommand(parent *cobra.Command, args []string) error {
	command := newInteractiveRootCommand()
	command.SetIn(parent.InOrStdin())
	command.SetOut(parent.OutOrStdout())
	command.SetErr(parent.ErrOrStderr())
	resolved, ctx, err := preparePreferences(command, interactiveArgs(parent, args), parent.Context())
	if err != nil {
		return err
	}
	command.SetArgs(resolved)
	restore := selector.SuspendScreen(parent.ErrOrStderr())
	defer restore()
	return command.ExecuteContext(ctx)
}

const deliveredTransferKeyCleanupWarning = "Warning: File delivery completed, but local encryption-key cleanup could not be completed.\n"

func writeDeliveredTransferKeyCleanupWarning(writer io.Writer) {
	if writer != nil {
		_, _ = io.WriteString(writer, deliveredTransferKeyCleanupWarning)
	}
}

func sendCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "send <path>... --to <machine>",
		Short: "Send files to a machine's Paperboat Inbox",
		Args:  commandArgs(cobra.MinimumNArgs(1)),
		RunE: func(cobraCommand *cobra.Command, paths []string) error {
			jsonOutput, _ := cobraCommand.Flags().GetBool("json")
			destinationRef, _ := cobraCommand.Flags().GetString("to")
			sessionID, _ := cobraCommand.Flags().GetString("session")
			if sessionID == "" {
				sessionID = strings.TrimSpace(os.Getenv("PAPERBOAT_TERMINAL_SESSION_ID"))
			}
			ctx := actionContext(cobraCommand, paths)
			dependencies, err := buildDeps(ctx)
			if err != nil {
				return err
			}
			credential, err := dependencies.auth.Credential()
			if err != nil {
				return err
			}
			client, err := newWorkspaceAPIClient(ctx, dependencies.cfg.ServerURL, credential)
			if err != nil {
				return err
			}
			sourceMachineID, err := configuredMachineID()
			if err != nil {
				return err
			}
			if err := requireLocalDaemonService(ctx.Context, dependencies.cfg); err != nil {
				return fmt.Errorf("prepare local peer transport: %w", err)
			}
			var destination api.UserMachine
			if strings.TrimSpace(destinationRef) != "" {
				destination, err = resolveUserMachine(ctx.Context, client, destinationRef)
				if err != nil {
					return friendlyCommandError(err)
				}
			} else {
				var configured api.TransferDestinationDefault
				var defaultErr error
				if strings.TrimSpace(sessionID) != "" {
					configured, defaultErr = client.TerminalSessionTransferDestination(ctx.Context, sessionID)
				}
				if defaultErr == nil && !configured.Configured {
					configured, defaultErr = client.TransferDestinationDefault(ctx.Context)
				}
				if defaultErr != nil {
					return friendlyCommandError(defaultErr)
				}
				if configured.Configured && configured.Machine != nil {
					destination = *configured.Machine
				} else if sessionID != "" {
					eligible, eligibleErr := client.EligibleTerminalSessionTransferDestinations(ctx.Context, sessionID)
					if eligibleErr != nil {
						return friendlyCommandError(eligibleErr)
					}
					eligible = slices.DeleteFunc(eligible, func(machine api.UserMachine) bool { return machine.ID == sourceMachineID })
					switch len(eligible) {
					case 0:
						return errors.New("no eligible transfer destination is attached to the session")
					case 1:
						destination = eligible[0]
					default:
						if jsonOutput || !term.IsTerminal(int(os.Stdin.Fd())) {
							summaries := make([]string, len(eligible))
							for i, machine := range eligible {
								summaries[i] = machine.Alias + " (" + machine.ID + ")"
							}
							return fmt.Errorf("transfer destination is ambiguous; use --to or set a default; eligible machines: %s", strings.Join(summaries, ", "))
						}
						index, promptErr := chooseIndex(cobraCommand.Context(), "Choose a transfer destination", "Eligible machines for this session", len(eligible), func(index int) selector.Item {
							machine := eligible[index]
							return selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(cobraCommand.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machineStatusSearch(machine)}
						})
						if promptErr != nil {
							return promptErr
						}
						destination = eligible[index]
					}
				} else {
					return errors.New("no default transfer destination is configured; use --to or `pb send destination set <machine>`")
				}
			}
			if destination.ID == sourceMachineID {
				return errors.New("destination must be a different machine")
			}
			if destination.State == "revoked" || destination.State == "disconnected" || destination.State == "deleted" {
				return errors.New("destination machine is revoked")
			}
			if sessionID == "" && !destination.Capabilities.FileReceive.Configured {
				return &api.APIError{Code: "machine_capability_unavailable", Message: "This machine is not configured to receive files."}
			}
			if sessionID == "" && (!destination.Online || !destination.Capabilities.FileReceive.Observed) {
				return &api.APIError{Code: "machine_offline", Message: "The destination machine is offline."}
			}
			if !jsonOutput {
				fmt.Fprintf(cobraCommand.ErrOrStderr(), "Sending to %s (%s)\n", destination.Alias, destination.ID)
			}
			preparedPaths := make([]string, len(paths))
			for i, path := range paths {
				preparedPaths[i], err = filepath.Abs(path)
				if err != nil {
					return err
				}
			}
			prepared, err := filetransfer.Prepare(preparedPaths, filetransfer.Limits{})
			if err != nil {
				return err
			}
			defer prepared.Close()
			batchID, err := filetransfer.NewBatchID()
			if err != nil {
				return err
			}
			requestFiles := make([]api.TeamInboxFile, len(prepared.Sources))
			for i, source := range prepared.Sources {
				requestFiles[i] = api.TeamInboxFile{Basename: source.Basename, Size: source.Size, SHA256: hex.EncodeToString(source.SHA256[:])}
			}
			requestUUID, err := uuid.NewRandom()
			if err != nil {
				return err
			}
			requestID := "request_" + requestUUID.String()
			operationID := newIdempotencyKey()
			acceptance, err := client.CreateTeamInboxRequest(ctx.Context, api.TeamInboxRequest{RequestID: requestID, SourceMachineID: sourceMachineID, DestinationMachineID: destination.ID, BatchID: batchID, Files: requestFiles, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, operationID)
			if err != nil {
				return friendlyCommandError(err)
			}
			if acceptance.Status == "pending" {
				if !jsonOutput {
					fmt.Fprintf(cobraCommand.ErrOrStderr(), "Waiting for recipient approval (%s).\n", acceptance.RequestID)
				}
				for acceptance.Status == "pending" {
					select {
					case <-ctx.Context.Done():
						return ctx.Context.Err()
					case <-time.After(2 * time.Second):
					}
					acceptance, err = client.TeamInboxRequest(ctx.Context, acceptance.RequestID)
					if err != nil {
						return friendlyCommandError(err)
					}
				}
			}
			if acceptance.Status != "approved" && acceptance.Status != "not_required" {
				return fmt.Errorf("file transfer was not accepted: %s", acceptance.Status)
			}
			descriptorRequestID, descriptorDigest := acceptance.RequestID, acceptance.ManifestDigest
			if acceptance.Status == "not_required" {
				descriptorRequestID, descriptorDigest = "", ""
			}
			descriptor, err := client.MachineFileTransferDescriptorForRequest(ctx.Context, destination.ID, sourceMachineID, sessionID, descriptorRequestID, descriptorDigest)
			if err != nil {
				return friendlyCommandError(err)
			}
			target := &resolver.FileTransferTarget{
				Endpoint: descriptor.Endpoint, SourceMachineID: descriptor.SourceMachineID,
				DestinationMachineID: descriptor.DestinationMachineID, InitiatingUserID: descriptor.InitiatingUserID,
				Auth:   resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.UTC().Format(time.RFC3339Nano), ResourceID: descriptor.Auth.AccessSessionID},
				Policy: descriptor.Policy,
			}
			if err := filetransfer.ValidateSources(prepared.Sources, fileTransferLimits(target)); err != nil {
				return err
			}
			if target.Endpoint == "" || target.Auth.Method != "bearer" || target.Auth.Token == "" {
				return errors.New("server returned an invalid file transfer descriptor")
			}
			retention := time.Duration(target.Policy.RetentionSeconds) * time.Second
			if retention <= 0 || retention > 7*24*time.Hour {
				retention = 7 * 24 * time.Hour
			}
			expiresAt := time.Now().UTC().Truncate(time.Second).Add(retention)
			var batch filetransfer.Batch
			if localSender, localErr := localFileTransferSenderFromEnvironment(); localErr != nil {
				return localErr
			} else if localSender != nil {
				if sessionID == "" {
					return errors.New("host-local file delivery requires a Paperboat terminal session")
				}
				batch, err = localSender.SendNativeBatch(ctx.Context, batchID, sourceMachineID, destination.ID, descriptor.InitiatingUserID, sessionID, prepared.Sources, expiresAt)
			} else {
				if dependencies.peerLocal == nil {
					return errors.New("local daemon transport is unavailable for native file transfer")
				}
				lease, leaseErr := dependencies.peerLocal.PrepareFileTransfer(ctx.Context, localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: destination.ID, EnvironmentID: destination.EnvironmentID, MachineGeneration: uint64(destination.InstallationGeneration), OperationID: batchID, Credential: target.Auth.Token, AccessSessionID: target.Auth.ResourceID, Deadline: parseAuthExpiry(target.Auth.ExpiresAt), MaximumBytes: uint64(target.Policy.MaxFileBytes)})
				if leaseErr != nil {
					return leaseErr
				}
				defer lease.Close()
				nativeClient, nativeErr := filetransfer.NewNativeClient(target.Endpoint, filetransfer.Auth{Token: target.Auth.Token, ExpiresAt: parseAuthExpiry(target.Auth.ExpiresAt)}, filetransfer.Binding{SourceMachineID: target.SourceMachineID, DestinationMachineID: target.DestinationMachineID, InitiatingUserID: target.InitiatingUserID}, lease.OpenTransferStream)
				if nativeErr != nil {
					return nativeErr
				}
				batch, err = nativeClient.SendBatch(ctx.Context, batchID, sessionID, prepared.Sources)
			}
			if err != nil {
				return err
			}
			if acceptance.Status == "approved" {
				if err := client.CompleteTeamInboxRequest(ctx.Context, acceptance.RequestID, acceptance.ManifestDigest); err != nil {
					reportDeliveredReceiptFailure(ctx.Context, cobraCommand.ErrOrStderr(), jsonOutput, err)
				}
			}
			if jsonOutput {
				return json.NewEncoder(cobraCommand.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": batch})
			}
			for i, item := range batch.Transfers {
				path := item.ReceiptPath
				if i < len(batch.Paths) && batch.Paths[i] != "" {
					path = batch.Paths[i]
				}
				fmt.Fprintf(cobraCommand.OutOrStdout(), "%s: delivered to %s on %s\n", item.Basename, path, destination.Alias)
			}
			return nil
		},
	}
	command.Flags().String("to", "", "destination machine name or ID")
	command.Flags().String("session", "", "terminal session ID for destination context")
	command.Flags().Bool("json", false, "print JSON")
	addSendManagementCommands(command)
	return command
}

func addSendManagementCommands(root *cobra.Command) {
	destination := &cobra.Command{Use: "destination", Short: "Show the default transfer destination", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		sessionID, _ := command.Flags().GetString("session")
		var value api.TransferDestinationDefault
		if sessionID == "" {
			value, err = client.TransferDestinationDefault(ctx.Context)
		} else {
			value, err = client.TerminalSessionTransferDestination(ctx.Context, sessionID)
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": value})
		}
		if !value.Configured || value.Machine == nil {
			fmt.Fprintln(command.OutOrStdout(), "No default transfer destination.")
			return nil
		}
		fmt.Fprintf(command.OutOrStdout(), "%s (%s)\n", value.Machine.Alias, value.Machine.ID)
		return nil
	}}
	destination.Flags().Bool("json", false, "print JSON")
	set := &cobra.Command{Use: "set <machine>", Short: "Set the default transfer destination", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machine, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		sessionID, _ := command.Flags().GetString("session")
		var value api.TransferDestinationDefault
		if sessionID == "" {
			value, err = client.SetTransferDestinationDefault(ctx.Context, machine.ID)
		} else {
			value, err = client.SetTerminalSessionTransferDestination(ctx.Context, sessionID, machine.ID)
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": value})
		}
		fmt.Fprintf(command.OutOrStdout(), "Default transfer destination: %s (%s)\n", machine.Alias, machine.ID)
		return nil
	}}
	set.Flags().Bool("json", false, "print JSON")
	clear := &cobra.Command{Use: "clear", Short: "Clear the default transfer destination", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		sessionID, _ := command.Flags().GetString("session")
		if sessionID == "" {
			err = client.ClearTransferDestinationDefault(ctx.Context)
		} else {
			err = client.ClearTerminalSessionTransferDestination(ctx.Context, sessionID)
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]bool{"configured": false}})
		}
		fmt.Fprintln(command.OutOrStdout(), "Default transfer destination cleared.")
		return nil
	}}
	clear.Flags().Bool("json", false, "print JSON")
	destination.AddCommand(set, clear)
	destination.PersistentFlags().String("session", "", "terminal session ID for a session-specific destination")
	status := &cobra.Command{Use: "status <transfer-id>", Short: "Inspect a file transfer", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, _, lease, err := transferClientForCommand(command, args)
		if err != nil {
			return err
		}
		defer lease.Close()
		manifest, err := client.Status(command.Context(), args[0])
		if err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": manifest})
		}
		fmt.Fprintf(command.OutOrStdout(), "%s  %s  %s -> %s\n", manifest.TransferID, manifest.State, manifest.SourceMachineID, manifest.DestinationMachineID)
		return nil
	}}
	cancelTransfer := &cobra.Command{Use: "cancel <transfer-id>", Short: "Cancel a file transfer batch", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, destination, lease, err := transferClientForCommand(command, args)
		if err != nil {
			return err
		}
		defer lease.Close()
		if err := client.Cancel(command.Context(), args[0]); err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]any{"transfer_id": args[0], "state": "canceled", "destination_machine_id": destination.ID}})
		}
		fmt.Fprintf(command.OutOrStdout(), "%s: canceled\n", args[0])
		return nil
	}}
	list := &cobra.Command{Use: "list", Short: "List file transfers", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, _, lease, err := transferClientForCommand(command, args)
		if err != nil {
			return err
		}
		defer lease.Close()
		sessionID, _ := command.Flags().GetString("session")
		limit, _ := command.Flags().GetInt("limit")
		offset, _ := command.Flags().GetInt("offset")
		query, _ := command.Flags().GetString("q")
		state, _ := command.Flags().GetString("state")
		page, err := client.ListPage(command.Context(), sessionID, limit, offset, query, state)
		if err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": page})
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "TRANSFER\tSTATE\tFILE\tDESTINATION")
		for _, item := range page.Items {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", item.TransferID, item.State, item.Basename, item.DestinationMachineID)
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		if page.Pagination.NextOffset != nil {
			destination, _ := command.Flags().GetString("on")
			operands := []string{"--on", destination, "--offset", strconv.Itoa(*page.Pagination.NextOffset), "--limit", strconv.Itoa(limit)}
			if sessionID != "" {
				operands = append(operands, "--session", sessionID)
			}
			if query != "" {
				operands = append(operands, "--q", query)
			}
			if state != "" {
				operands = append(operands, "--state", state)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Next page: pb send list %s\n", formatPreferenceArgs(operands))
			return err
		}
		return nil
	}}
	list.Flags().Int("limit", 50, "maximum transfers to return")
	list.Flags().Int("offset", 0, "continue at a transfer history offset")
	list.Flags().String("q", "", "filter by file basename or transfer ID")
	list.Flags().String("state", "", "filter by transfer state")
	for _, command := range []*cobra.Command{list, status, cancelTransfer} {
		command.Flags().String("on", "", "destination machine name or ID")
		command.Flags().String("session", "", "terminal session ID for destination context")
		command.Flags().Bool("json", false, "print JSON")
	}
	root.AddCommand(destination, list, status, cancelTransfer)
}

func transferClientForCommand(cobraCommand *cobra.Command, args []string) (*filetransfer.NativeClient, api.UserMachine, io.Closer, error) {
	destinationRef, _ := cobraCommand.Flags().GetString("on")
	if strings.TrimSpace(destinationRef) == "" {
		return nil, api.UserMachine{}, nil, invocationError(errors.New("--on is required"))
	}
	ctx := actionContext(cobraCommand, args)
	backend, err := backendClient(ctx)
	if err != nil {
		return nil, api.UserMachine{}, nil, err
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return nil, api.UserMachine{}, nil, err
	}
	destination, err := resolveUserMachine(ctx.Context, backend, destinationRef)
	if err != nil {
		return nil, api.UserMachine{}, nil, friendlyCommandError(err)
	}
	sessionID, _ := cobraCommand.Flags().GetString("session")
	descriptor, err := backend.MachineFileTransferDescriptor(ctx.Context, destination.ID, sourceMachineID, sessionID)
	if err != nil {
		return nil, api.UserMachine{}, nil, friendlyCommandError(err)
	}
	target := &resolver.FileTransferTarget{Endpoint: descriptor.Endpoint, SourceMachineID: descriptor.SourceMachineID, DestinationMachineID: descriptor.DestinationMachineID, InitiatingUserID: descriptor.InitiatingUserID, Auth: resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.UTC().Format(time.RFC3339Nano), ResourceID: descriptor.Auth.AccessSessionID}, Policy: descriptor.Policy}
	client, lease, err := newTransferCommandClient(ctx.Context, target, destination, newIdempotencyKey())
	if err != nil {
		return nil, api.UserMachine{}, nil, err
	}
	return client, destination, lease, nil
}

func addConnectFlags(command *cobra.Command) {
	command.Flags().String("name", "", "name for the fresh terminal session")
	command.Flags().String("session", "", "attach an existing terminal session by name or ID")
	command.Flags().Bool("debug", false, "show the pb versions used by this terminal session")
	addStatusBarFlags(command)
}

func addStatusBarFlags(command *cobra.Command) {
	command.Flags().String("status-bar", "", "status bar for this attach: auto, on, or off")
	command.Flags().String("status-bar-fullscreen", "", "status bar in full-screen applications: hide or show")
	command.Flags().String("status-bar-theme", "", "status bar theme: terminal, dark, light, or mono")
}

func validateConnectInvocation(command *cobra.Command) error {
	name, _ := command.Flags().GetString("name")
	ref, _ := command.Flags().GetString("session")
	if strings.TrimSpace(name) != "" && strings.TrimSpace(ref) != "" {
		return invocationError(errors.New("--name and --session cannot be used together"))
	}
	for name, allowed := range map[string][]string{
		"status-bar":            {"auto", "on", "off"},
		"status-bar-fullscreen": {"hide", "show"},
		"status-bar-theme":      {"terminal", "dark", "light", "mono"},
	} {
		value, _ := command.Flags().GetString(name)
		if value != "" && !containsString(allowed, strings.ToLower(strings.TrimSpace(value))) {
			return invocationError(fmt.Errorf("--%s must be one of %s", name, strings.Join(allowed, ", ")))
		}
	}
	server, _ := command.Flags().GetString("server")
	if strings.TrimSpace(server) != "" {
		if _, err := config.NormalizeServerURL(server); err != nil {
			return invocationError(err)
		}
	}
	return nil
}

func specTree(source *command.Spec, use string) *cobra.Command {
	root := &cobra.Command{Use: use, Short: source.Usage, Args: commandArgs(cobra.NoArgs)}
	if source.Action != nil {
		root.RunE = actionRun(source.Action)
	} else {
		root.RunE = func(command *cobra.Command, _ []string) error { return command.Help() }
	}
	for _, child := range source.Subcommands {
		child := child
		childUse := child.Name
		if child.ArgsUsage != "" {
			childUse += " " + child.ArgsUsage
		}
		entry := &cobra.Command{Use: childUse, Short: child.Usage, Args: commandArgs(specCommandArgs(use, child.Name)), RunE: actionRun(child.Action)}
		for _, configuredFlag := range child.Flags {
			switch configuredFlag := configuredFlag.(type) {
			case *command.StringFlag:
				entry.Flags().String(configuredFlag.Name, "", configuredFlag.Usage)
			case *command.BoolFlag:
				entry.Flags().Bool(configuredFlag.Name, false, configuredFlag.Usage)
			case *command.UintFlag:
				entry.Flags().Uint(configuredFlag.Name, 0, configuredFlag.Usage)
			case *command.Float64Flag:
				entry.Flags().Float64(configuredFlag.Name, 0, configuredFlag.Usage)
			}
		}
		if use == "config" && child.Name == "status" {
			entry.Flags().Bool("json", false, "print JSON")
		}
		if use == "config" && child.Name == "unassign" {
			entry.Flags().Bool("json", false, "print JSON")
		}

		if use == "config" && child.Name == "unassign" {
			entry.Flags().String("confirm", "", "six-character confirmation code from the preview")
		}
		root.AddCommand(entry)
	}
	return root
}

func specCommandArgs(parent, name string) cobra.PositionalArgs {
	if parent == "inbox" {
		switch name {
		case "set":
			return cobra.ExactArgs(1)
		case "path", "reset":
			return cobra.NoArgs
		}
	}
	if parent == "config" {
		switch name {
		case "set":
			return cobra.ExactArgs(2)
		case "unset":
			return cobra.ExactArgs(1)
		case "assign", "enable":
			return cobra.ExactArgs(2)
		case "unassign", "disable":
			return cobra.ExactArgs(1)
		case "approve", "team-default-adopt":
			return cobra.ExactArgs(1)
		case "team-default-set":
			return cobra.ExactArgs(2)
		case "team-default-unadopt":
			return cobra.NoArgs
		case "status":
			return cobra.MaximumNArgs(1)
		}
	}
	if parent == "preview" {
		switch name {
		case "create":
			return cobra.MaximumNArgs(1)
		case "list":
			return cobra.NoArgs
		case "revoke":
			return cobra.MaximumNArgs(1)
		}
	}
	return cobra.NoArgs
}

func sessionCobraCommand() *cobra.Command {
	source := sessionsCommand()
	command := &cobra.Command{Use: "session", Short: source.Usage, Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			return actionHomeSessions(command)
		}
		return command.Help()
	}}
	attach := &cobra.Command{Use: "attach [environment] [session]", Short: "Choose and attach to a durable terminal session", Args: commandArgs(cobra.MaximumNArgs(2)), RunE: func(cobraCommand *cobra.Command, args []string) error {
		if err := validateConnectInvocation(cobraCommand); err != nil {
			return err
		}
		ctx := actionContext(cobraCommand, args)
		d, err := buildDeps(ctx)
		if err != nil {
			return err
		}
		credential, err := d.auth.Credential()
		if err != nil {
			return err
		}
		client, err := newWorkspaceAPIClient(ctx, d.cfg.ServerURL, credential)
		if err != nil {
			return err
		}
		environmentRef := ""
		sessionRef := ""
		switch len(args) {
		case 2:
			environmentRef, sessionRef = args[0], args[1]
		case 1:
			sessionRef = args[0]
			environmentRef, err = defaultWorkspaceEnvironment(ctx.Context, ctx, client, d.cfg)
		default:
			environmentRef, err = selectEnvironment(ctx.Context, client, "Choose an environment")
		}
		if err != nil {
			return err
		}
		target, err := resolveTerminalEnvironmentTarget(ctx.Context, client, environmentRef)
		if err != nil {
			return err
		}
		if sessionRef == "" {
			sessions, listErr := listTerminalSessionsForTarget(ctx.Context, client, target)
			if listErr != nil {
				return friendlyCommandError(listErr)
			}
			selected, selectErr := selectSession(ctx.Context, client, target, sessions, "Choose a terminal session")
			if selectErr != nil {
				return selectErr
			}
			sessionRef = selected.ID
		}
		if err := cobraCommand.Flags().Set("session", sessionRef); err != nil {
			return err
		}
		return actionConnectTarget(actionContext(cobraCommand, nil), environmentRef)
	}}
	attach.Flags().String("session", "", "")
	_ = attach.Flags().MarkHidden("session")
	attach.Flags().Bool("debug", false, "show the pb versions used by this terminal session")
	addStatusBarFlags(attach)
	command.AddCommand(attach)
	list := &cobra.Command{Use: "list [environment]", Short: "List durable terminal sessions", Args: commandArgs(cobra.MaximumNArgs(1)), RunE: actionRun(source.Action)}
	list.Flags().Bool("wide", false, "include immutable IDs")
	list.Flags().Bool("json", false, "print JSON")
	inventoryFilterFlags(list)
	list.Flags().Bool("closed", false, "filter explicitly by closed state")
	command.AddCommand(list)
	for _, child := range source.Subcommands {
		child := child
		var args cobra.PositionalArgs
		switch child.Name {
		case "rename":
			args = cobra.ExactArgs(3)
		case "close":
			args = cobra.RangeArgs(1, 2)
		case "delete":
			args = cobra.RangeArgs(1, 2)
		}
		entry := &cobra.Command{Use: child.Name, Short: child.Usage, Args: commandArgs(args), RunE: actionRun(child.Action)}
		if child.Name == "rename" {
			entry.Flags().Bool("json", false, "print JSON")
		}
		if child.Name == "close" || child.Name == "delete" {
			entry.Flags().String("confirm", "", "six-character confirmation code from the preview")
			entry.Flags().Bool("json", false, "print JSON")
		}
		if child.Name == "close" || child.Name == "delete" {
			entry.Flags().Bool("all", false, child.Name+" all sessions in the environment")
		}
		command.AddCommand(entry)
	}
	addTerminalSharingCommands(command)
	return command
}

func userMachineCobraCommand() *cobra.Command {
	machine := &cobra.Command{Use: "machine", Short: "Manage machines", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			return actionHomeMachines(command)
		}
		return command.Help()
	}}
	add := &cobra.Command{Use: "add", Short: "Print Linux/macOS and Windows machine enrollment commands", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		cfg, err := config.Load(ctx.String("config"))
		if err != nil {
			return err
		}
		if server := strings.TrimSpace(ctx.String("server")); server != "" {
			cfg.ServerURL, err = config.NormalizeServerURL(server)
			if err != nil {
				return err
			}
		}
		if strings.TrimSpace(cfg.ServerURL) == "" {
			return commandRejection{reason: commandRejectMissingServer}
		}
		name, _ := command.Flags().GetString("name")
		authSource, err := sessionauth.NewSource(cfg)
		if err != nil {
			return err
		}
		credential, err := authSource.Credential()
		if err != nil {
			return err
		}
		client := api.New(cfg.ServerURL, credential, nil)
		result, err := client.StartMachineEnrollment(ctx.Context, newIdempotencyKey())
		if err != nil {
			return friendlyCommandError(err)
		}
		parameter := result.BootstrapToken
		if name != "" {
			parameter = name + "-" + parameter
		}
		windowsURL := "https://get.pprbt.dev/install?p=" + parameter
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"expires_at": result.ExpiresAt, "linux_macos_command": "curl -fsSL 'https://get.pprbt.dev/install?p=" + parameter + "' | bash", "windows_command": windowsEnrollmentCommand(windowsURL)})
		}
		fmt.Fprintf(command.OutOrStdout(), "Linux/macOS:\ncurl -fsSL 'https://get.pprbt.dev/install?p=%s' | bash\n\nWindows (PowerShell or Command Prompt):\n%s\n", parameter, windowsEnrollmentCommand(windowsURL))
		return nil
	}}
	add.Flags().String("name", "", "optional machine hostname")
	add.Flags().Bool("json", false, "print JSON")
	list := &cobra.Command{Use: "list", Short: "List enrolled machines", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machines, err := client.ListUserMachinesFiltered(ctx.Context, inventoryFilters(command))
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machines": machines})
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tOWNERSHIP\tSTATE\tPERMISSIONS\tID")
		for _, item := range machines {
			state := item.State
			if item.Online {
				state = "online"
			}
			ownership := item.Ownership
			if item.OwnerTeamID != "" {
				ownership = "team:" + item.OwnerTeamID
			}
			permissions := strings.Join(item.Permissions, ",")
			if item.CanManage {
				permissions = strings.Trim(permissions+",manage", ",")
			}
			if permissions == "" {
				permissions = "none"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", item.Alias, ownership, state, permissions, item.ID)
		}
		return writer.Flush()
	}}
	list.Flags().Bool("json", false, "print JSON")
	inventoryFilterFlags(list)
	rename := &cobra.Command{Use: "rename <machine> <name>", Short: "Rename a machine", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(command *cobra.Command, args []string) error {
		newName := strings.TrimSpace(args[1])
		if err := machinename.Validate(newName); err != nil {
			return invocationError(fmt.Errorf("invalid machine name: %w", err))
		}
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machine, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if !jsonOutput {
			fmt.Fprintf(command.ErrOrStderr(), "Machine: %s (%s)\n", machine.Alias, machine.ID)
		}
		updated, err := client.RenameUserMachine(ctx.Context, machine.ID, newName)
		if err != nil {
			return friendlyCommandError(err)
		}
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": updated, "outcome": "renamed"})
		}
		fmt.Fprintf(command.OutOrStdout(), "Renamed machine %s to %s (%s).\n", machine.Alias, updated.Alias, updated.ID)
		return nil
	}}
	rename.Flags().Bool("json", false, "print JSON")
	revoke := &cobra.Command{Use: "revoke <machine>", Short: "Disconnect and revoke a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(cobraCommand *cobra.Command, args []string) error {
		ctx := actionContext(cobraCommand, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		userMachineID, alias, err := resolveUserMachineTarget(ctx.Context, client, args[0])
		if err != nil {
			return err
		}
		if err := confirmMutation(cobraCommand, "machine-revoke:"+userMachineID, fmt.Sprintf("Revoke machine %s (%s)? Its enrollment and credentials will lose authority.", alias, userMachineID)); err != nil {
			return err
		}
		if err := client.DisconnectUserMachine(ctx.Context, userMachineID); err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := cobraCommand.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(cobraCommand.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": map[string]string{"id": userMachineID, "alias": alias, "state": "disconnected"}, "outcome": "confirmed", "retry": "not_required"})
		}
		fmt.Fprintf(cobraCommand.OutOrStdout(), "Disconnected machine %s (%s).\n", alias, userMachineID)
		return nil
	}}
	revoke.Flags().String("confirm", "", "six-character confirmation code from the preview")
	revoke.Flags().Bool("json", false, "print JSON")
	availability := &cobra.Command{Use: "availability <machine>", Short: "Set machine sleep availability", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		modeFlag, _ := command.Flags().GetString("mode")
		mode := strings.ReplaceAll(strings.TrimSpace(modeFlag), "-", "_")
		if mode != "allow_sleep" && mode != "keep_awake" {
			return localArgumentError("availability --mode must be allow-sleep or keep-awake")
		}
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machine, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		if mode == "keep_awake" {
			if err := confirmMutation(command, fmt.Sprintf("machine-keep-awake:%s:%d", machine.ID, machine.Availability.DesiredVersion), fmt.Sprintf("Keep machine %s (%s) available while idle? This can increase battery use and heat and keep a closed-lid machine awake.", machine.Alias, machine.ID)); err != nil {
				return err
			}
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if !jsonOutput {
			fmt.Fprintf(command.ErrOrStderr(), "Machine: %s (%s)\n", machine.Alias, machine.ID)
		}
		policy, err := client.SetUserMachineAvailability(ctx.Context, machine.ID, mode, newIdempotencyKey(), machine.Availability.DesiredVersion)
		if err != nil {
			return friendlyCommandError(err)
		}
		policy = waitForAvailabilityObservation(ctx.Context, client, machine.ID, policy, 5*time.Second)
		outcome := "pending"
		if policy.Status == "applied" && policy.ObservedVersion == policy.DesiredVersion && policy.ObservedMode == policy.DesiredMode {
			outcome = "applied"
		}
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": map[string]string{"id": machine.ID, "alias": machine.Alias}, "availability": policy, "outcome": outcome, "retry": "automatic"})
		}
		if outcome == "applied" {
			fmt.Fprintf(command.OutOrStdout(), "Availability %s applied to %s.\n", strings.ReplaceAll(mode, "_", "-"), machine.Alias)
		} else {
			fmt.Fprintf(command.OutOrStdout(), "Availability %s saved for %s; application is durably %s.\n", strings.ReplaceAll(mode, "_", "-"), machine.Alias, policy.Status)
		}
		return nil
	}}
	availability.Flags().String("mode", "", "availability mode: allow-sleep or keep-awake")
	availability.Flags().String("confirm", "", "six-character confirmation code for keep-awake mode")
	availability.Flags().Bool("json", false, "print JSON")
	capabilities := &cobra.Command{Use: "capabilities <machine>", Short: "Set incoming services for a machine", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		overrides := make(map[string]bool, 4)
		for _, name := range []string{"terminal", "managed-ssh", "file-receive", "preview-tunnel"} {
			if command.Flags().Changed(name) {
				value, err := command.Flags().GetBool(name)
				if err != nil {
					return invocationError(err)
				}
				overrides[name] = value
			}
		}
		if len(overrides) == 0 {
			return localArgumentError("set at least one capability flag")
		}
		ctx := actionContext(command, args)
		client, err := backendClient(ctx)
		if err != nil {
			return err
		}
		machineValue, err := resolveUserMachine(ctx.Context, client, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		desired := machineValue.MachineCapabilities.Desired
		for name, destination := range map[string]*bool{"terminal": &desired.Terminal, "managed-ssh": &desired.ManagedSSH, "file-receive": &desired.FileReceive, "preview-tunnel": &desired.PreviewTunnel} {
			if value, changed := overrides[name]; changed {
				*destination = value
			}
		}
		policy, err := client.SetUserMachineCapabilities(ctx.Context, machineValue.ID, newIdempotencyKey(), desired, machineValue.MachineCapabilities.DesiredVersion)
		if err != nil {
			return friendlyCommandError(err)
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"version": "1", "machine": map[string]string{"id": machineValue.ID, "alias": machineValue.Alias}, "machine_capabilities": policy})
		}
		fmt.Fprintf(command.OutOrStdout(), "Incoming capabilities saved for %s; application is %s.\n", machineValue.Alias, policy.Status)
		return nil
	}}
	capabilities.Flags().Bool("terminal", false, "accept Paperboat terminal and exec")
	capabilities.Flags().Bool("managed-ssh", false, "accept managed SSH/SCP/SFTP/rsync")
	capabilities.Flags().Bool("file-receive", false, "accept native Inbox transfers")
	capabilities.Flags().Bool("preview-tunnel", false, "serve preview and tunnel routes")
	capabilities.Flags().Bool("json", false, "print JSON")
	machine.AddCommand(add, list, rename, revoke, availability, capabilities)
	return machine
}

func windowsEnrollmentCommand(installerURL string) string {
	return "powershell -c \"iex (irm '" + installerURL + "')\""
}

func waitForAvailabilityObservation(ctx context.Context, client *api.Client, machineID string, current api.AvailabilityPolicy, timeout time.Duration) api.AvailabilityPolicy {
	if current.Status == "applied" && current.ObservedVersion == current.DesiredVersion && current.ObservedMode == current.DesiredMode {
		return current
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return current
		case <-ticker.C:
			machines, err := client.ListUserMachines(waitCtx)
			if err != nil {
				continue
			}
			for _, machine := range machines {
				if machine.ID != machineID || machine.Availability.DesiredVersion != current.DesiredVersion {
					continue
				}
				current = machine.Availability
				if current.Status == "applied" || current.Status == "unsupported" || current.Status == "error" || current.Status == "offline" {
					return current
				}
			}
		}
	}
}

func resolveUserMachineTarget(ctx context.Context, client *api.Client, requested string) (string, string, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return "", "", friendlyCommandError(err)
	}
	for _, machine := range machines {
		if machine.ID == requested {
			return machine.ID, machine.Alias, nil
		}
	}
	matches := make([]api.UserMachine, 0, 1)
	for _, machine := range machines {
		if strings.EqualFold(machine.Alias, requested) {
			matches = append(matches, machine)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, matches[0].Alias, nil
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("machine name %q is ambiguous; use a stable machine ID", requested)
	}
	return "", "", fmt.Errorf("machine %q was not found", requested)
}

func actionRun(action command.Action) func(*cobra.Command, []string) error {
	return func(command *cobra.Command, args []string) error {
		c := actionContext(command, args)
		c.Context = context.WithValue(c.Context, confirmationCommandKey{}, command)
		return action(c)
	}
}

func actionContext(cobraCommand *cobra.Command, args []string) *command.Context {
	set := flag.NewFlagSet("pb", flag.ContinueOnError)
	values := map[string]string{}
	for _, name := range []string{"config", "server", "workspace", "name", "machine", "session", "status-bar", "status-bar-fullscreen", "status-bar-theme", "mode", "pull-repository", "push-repository", "path", "transport", "code", "input", "output", "keep", "recovery-key", "token-file"} {
		value, _ := cobraCommand.Flags().GetString(name)
		values[name] = value
		set.String(name, value, "")
	}
	hours, _ := cobraCommand.Flags().GetFloat64("hours")
	set.Float64("hours", hours, "")
	for _, name := range []string{"json", "no-browser", "reauth", "change-account", "wide", "automatic-updates", "clear", "all", "indefinite", "public", "detach", "select-environment", "debug"} {
		value, _ := cobraCommand.Flags().GetBool(name)
		values[name] = strconv.FormatBool(value)
		set.Bool(name, value, "")
	}
	port, _ := cobraCommand.Flags().GetUint("port")
	set.Uint("port", port, "")
	listenPort, _ := cobraCommand.Flags().GetUint("listen-port")
	set.Uint("listen-port", listenPort, "")
	generation, _ := cobraCommand.Flags().GetUint("generation")
	set.Uint("generation", generation, "")
	duration, _ := cobraCommand.Flags().GetDuration("duration")
	set.Duration("duration", duration, "")
	set.Bool("duration-set", cobraCommand.Flags().Changed("duration"), "")
	_ = set.Parse(args)
	context := command.NewContext(set)
	context.Context = cobraCommand.Context()
	context.Writer = cobraCommand.OutOrStdout()
	context.ErrWriter = cobraCommand.ErrOrStderr()
	return context
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func authCommand() *command.Spec {
	return &command.Spec{Name: "auth", Usage: "Manage Paperboat sign-in", Subcommands: []*command.Spec{
		{Name: "login", Usage: "Sign in through browser approval on any device", Flags: []command.Flag{&command.BoolFlag{Name: "no-browser", Usage: "print the approval link without opening a browser"}, &command.BoolFlag{Name: "reauth", Usage: "authenticate the current account again"}, &command.BoolFlag{Name: "change-account", Usage: "sign in with another account after browser approval"}, &command.BoolFlag{Name: "json", Usage: "print approval and sign-in states as JSON"}}, Action: authBrowserLogin},
		{Name: "status", Usage: "Show the active Paperboat account", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: authStatus},
		{Name: "logout", Usage: "Revoke and remove the active client session", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: authLogout},
	}}
}

func requireAuthConfig(c *command.Context) (*config.Config, config.ProfileStore, error) {
	d, err := buildDeps(c)
	if err != nil {
		return nil, config.ProfileStore{}, err
	}
	if strings.TrimSpace(d.cfg.ServerURL) == "" {
		return nil, config.ProfileStore{}, commandRejection{reason: commandRejectMissingServer}
	}
	s, err := config.ProfileStoreFor(d.cfg)
	if err != nil {
		return nil, config.ProfileStore{}, err
	}
	return d.cfg, s, nil
}

const dashboardEnrollmentGuidance = "Run `pb login` and approve this CLI in your browser. The printed link works on another device when this machine has no browser."

func exportSetupRecoveryKey(command *cobra.Command) error {
	output, err := command.Flags().GetString("recovery-output")
	if err != nil || strings.TrimSpace(output) == "" {
		return err
	}
	output = strings.TrimSpace(output)
	if !filepath.IsAbs(output) {
		return invocationError(errors.New("--recovery-output must be an absolute path"))
	}
	ctx := actionContext(command, nil)
	_, store, profile, err := e2eeClient(ctx)
	if err != nil {
		return fmt.Errorf("machine setup completed, but recovery-key export failed: %w", err)
	}
	seed, err := store.ExportPeerAccountRootSeed(profile.Issuer, profile.Account.ID)
	if err != nil {
		return fmt.Errorf("machine setup completed, but recovery-key export failed: %w", err)
	}
	defer clear(seed)
	encoded, err := recoverykey.Encode(seed)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("machine setup completed, but create recovery-key file: %w", err)
	}
	if _, err = io.WriteString(file, encoded+"\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("machine setup completed, but write recovery-key file: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("machine setup completed, but close recovery-key file: %w", err)
	}
	if !jsonOutputRequested(command) {
		fmt.Fprintf(command.OutOrStdout(), "Recovery key written to %s\n", output)
	}
	return nil
}

func e2eeClient(c *command.Context) (*api.Client, config.ProfileStore, config.Profile, error) {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	if server := strings.TrimSpace(c.String("server")); server != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(server)
		if err != nil {
			return nil, config.ProfileStore{}, config.Profile{}, err
		}
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return nil, config.ProfileStore{}, config.Profile{}, commandRejection{reason: commandRejectMissingServer}
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	source := (&sessionauth.Source{Store: store, Issuer: cfg.ServerURL}).WithContext(c.Context)
	credential, err := source.Credential()
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	client, err := newWorkspaceAPIClient(c, cfg.ServerURL, credential)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	return client, store, profile, nil
}

func authStatus(c *command.Context) error {
	cfg, store, err := requireAuthConfig(c)
	if err != nil {
		return err
	}
	if err := store.Recover(cfg.ServerURL); err != nil {
		return fmt.Errorf("recover interrupted Paperboat sign-in: %w", err)
	}
	p, err := store.Load(cfg.ServerURL)
	if onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return leaf == config.ErrNoCredentials }) {
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(map[string]any{"signed_in": false})
		}
		fmt.Fprintln(c.Writer, "Not signed in")
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := store.CredentialFor(cfg.ServerURL); err != nil {
		return &sessionauth.CredentialFailure{Cause: err}
	}
	if c.Bool("json") {
		document := map[string]any{"signed_in": true, "issuer": p.Issuer, "cli_client_session_id": p.CLIClientSessionID, "access_expires_at": p.AccessExpiresAt, "account": p.Account}
		if root, rootErr := store.LoadPeerAccountRootPublic(p.Issuer, p.Account.ID); rootErr == nil {
			fingerprint, fingerprintErr := endpointidentity.RootFingerprint(root)
			if fingerprintErr != nil {
				return fingerprintErr
			}
			document["trusted_root_public_key"] = base64.RawURLEncoding.EncodeToString(root)
			document["trusted_root_fingerprint"] = fingerprint
		} else if !onlyEnvironmentFailureLeaves(rootErr, func(leaf error) bool { return leaf == config.ErrSecretNotFound }) {
			return fmt.Errorf("load locally trusted identity: %w", rootErr)
		}
		return json.NewEncoder(c.Writer).Encode(document)
	}
	fmt.Fprintf(c.Writer, "Signed in as %s\nServer: %s\nSession: %s\nAccess expires: %s\n", firstNonEmpty(p.Account.Email, p.Account.DisplayName, p.Account.ID), p.Issuer, p.CLIClientSessionID, p.AccessExpiresAt.Format(time.RFC3339))
	return nil
}

func authLogout(c *command.Context) error {
	cfg, store, err := requireAuthConfig(c)
	if err != nil {
		return err
	}
	if err := store.Recover(cfg.ServerURL); err != nil {
		return fmt.Errorf("recover interrupted Paperboat sign-in: %w", err)
	}
	return store.WithBrowserLogin(cfg.ServerURL, func(state *config.BrowserLoginState, save func() error) error {
		cancellationErr := cancelBrowserLogin(cfg.ServerURL, state, save)
		return authLogoutSessions(c, cfg, store, cancellationErr)
	})
}

func authLogoutSessions(c *command.Context, cfg *config.Config, store config.ProfileStore, cancellationErr error) error {
	credentials, err := store.TakeLogoutCredentials(cfg.ServerURL)
	partial, locallySignedOut := err.(*config.LogoutRevocationIncompleteError)
	if err != nil && (!locallySignedOut || partial == nil) {
		return fmt.Errorf("remove local Paperboat sessions: %w", err)
	}
	unconfirmed := locallySignedOut
	observe := func(cause error) {
		if cause != nil && !errorreport.HTTPAttemptObserved(cause) {
			if _, localRead := cause.(*config.LogoutRevocationIncompleteError); localRead {
				errorreport.Current().ObserveFailure(c.Context, "pb", "auth", "command", "command_failed", cause)
				return
			}
			errorreport.Current().ObserveFailure(c.Context, "pb", "auth", "control_request", "control_request_failed", cause)
		}
	}
	observe(err)
	revokeCtx, cancel := context.WithTimeout(c.Context, 2*time.Second)
	defer cancel()
	// Sequential requests share one deadline. No worker can outlive local
	// signout, and the number of historical sessions never grows concurrency.
	for _, credential := range credentials {
		if revokeCtx.Err() != nil {
			unconfirmed = true
			observe(revokeCtx.Err())
			break
		}
		if err := api.RevokeToken(revokeCtx, cfg.ServerURL, credential.RefreshToken, nil); err != nil {
			unconfirmed = true
			observe(err)
		}
	}
	if cancellationErr != nil {
		observe(cancellationErr)
	}
	status := "complete"
	if unconfirmed {
		status = "unconfirmed"
	}
	if c.Bool("json") {
		result := map[string]any{"signed_out": true, "server": cfg.ServerURL, "server_revocation": status}
		if cancellationErr != nil {
			result["browser_login_cancellation"] = "unconfirmed"
		}
		return writeCLIJSON(c.Writer, result)
	}
	if _, err := fmt.Fprintln(c.Writer, "Signed out"); err != nil {
		return err
	}
	if unconfirmed {
		if _, err := fmt.Fprintln(c.ErrWriter, "Some server sessions could not be confirmed as revoked. Local credentials were removed; manage active sessions in the Paperboat dashboard."); err != nil {
			return err
		}
	}
	if cancellationErr != nil {
		if _, err := fmt.Fprintln(c.ErrWriter, "Cancellation of an earlier browser login could not be confirmed. Check pending sign-in requests in the Paperboat dashboard."); err != nil {
			return err
		}
	}
	return nil
}

func drainPendingRevocations(ctx context.Context, issuer string, store config.ProfileStore) error {
	records, err := store.PendingRevocations(issuer)
	if err != nil {
		return err
	}
	var errs []error
	for _, record := range records {
		if record.Cancelled {
			if err := store.CompleteRevocation(record); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if record.ServerRevoked {
			if err := store.CompleteRevocation(record); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		cred, err := store.PendingRevocationCredential(record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := api.RevokeToken(ctx, issuer, cred.RefreshToken, nil); err != nil {
			errs = append(errs, fmt.Errorf("revoke client session %s: %w", record.CLIClientSessionID, err))
			continue
		}
		record, err = store.MarkRevocationSucceeded(record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := store.CompleteRevocation(record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var openBrowser = platformOpenBrowser

func backendClient(c *command.Context) (*api.Client, error) {
	d, err := buildDeps(c)
	if err != nil {
		return nil, err
	}
	if d.cfg.ServerURL == "" {
		return nil, commandRejection{reason: commandRejectMissingServer}
	}
	cred, err := d.auth.Credential()
	if err != nil {
		return nil, err
	}
	return newWorkspaceAPIClient(c, d.cfg.ServerURL, cred)
}

type environmentTarget struct {
	kind string
	id   string
	name string
}

const environmentUserMachine = "machine"

func selectEnvironment(ctx context.Context, client *api.Client, title string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("an environment is required in non-interactive use")
	}
	var machines []api.UserMachine
	load := func(loadCtx context.Context) error {
		var err error
		machines, err = client.ListUserMachines(loadCtx)
		return friendlyCommandError(err)
	}
	var err error
	if selector.ScreenActive() {
		err = selector.Loading(ctx, title, "Loading environments", os.Stdin, os.Stderr, load)
	} else {
		err = load(ctx)
	}
	if err != nil {
		return "", err
	}
	machines = terminalHostMachines(machines)
	items := make([]selector.Item, 0, len(machines))
	for _, machine := range machines {
		if !machine.Capabilities.TerminalHost.Configured || !machine.Capabilities.TerminalHost.Observed {
			continue
		}
		items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(ctx, "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: "machine " + machineStatusSearch(machine)})
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Title: title, Subtitle: "Terminal-capable machines", Items: items, Empty: "no terminal-capable machines are available; run `pb setup` or `pb machine add`", Stdin: os.Stdin, Output: os.Stderr})
	return selected.ID, err
}

func selectSession(ctx context.Context, client *api.Client, target environmentTarget, sessions []api.TerminalSession, title string) (api.TerminalSession, error) {
	items := make([]selector.Item, 0, len(sessions))
	byID := make(map[string]api.TerminalSession, len(sessions))
	for _, session := range sessions {
		attached := "no attachments"
		if session.AttachedCount != nil {
			attached = fmt.Sprintf("%d attached", *session.AttachedCount)
		}
		activity := "last active " + relativeTime(session.LastActiveAt)
		created := "created " + relativeTimestamp(session.CreatedAt)
		description := sessionIdentificationDetails(session, preferenceDetails(ctx, "sessions", map[string]string{"machine": target.name, "state": strings.Join([]string{session.State, attached, activity, created}, " · "), "id": session.ID}))
		items = append(items, selector.Item{ID: session.ID, Title: session.Name, Description: description, Search: target.name + " " + session.State + " " + sessionIdentificationSearch(session)})
		byID[session.ID] = session
	}
	refresh := func(refreshCtx context.Context) ([]selector.Item, error) {
		fresh, err := listTerminalSessionsForTarget(refreshCtx, client, target)
		if err != nil {
			return nil, err
		}
		updated := make([]selector.Item, 0, len(fresh))
		for _, entry := range fresh {
			if _, ok := byID[entry.ID]; !ok {
				continue
			}
			updated = append(updated, selector.Item{ID: entry.ID, Title: entry.Name, Description: sessionIdentificationDetails(entry, target.name+" · "+entry.State), Search: sessionIdentificationSearch(entry)})
		}
		return updated, nil
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Refresh: refresh, Title: title, Subtitle: target.name + "  ·  " + target.kind, Items: items, Empty: "no terminal sessions are available", Stdin: os.Stdin, Output: os.Stderr})
	return byID[selected.ID], err
}

func relativeTimestamp(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	value := at
	return relativeTime(&value)
}

func defaultEnvironment(ctx context.Context, client *api.Client, rememberedID, workspace string) (string, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return "", friendlyCommandError(err)
	}
	machines = terminalHostMachines(machines)
	if rememberedID = strings.TrimSpace(rememberedID); rememberedID != "" {
		for _, machine := range machines {
			if machine.ID == rememberedID {
				return rememberedID, nil
			}
		}
		return "", fmt.Errorf("last environment %q is not available in the selected %s workspace; choose a machine in this workspace or run `pb switch <workspace>`", rememberedID, workspaceDisplayName(workspace))
	}
	if len(machines) == 1 {
		return machines[0].ID, nil
	}
	if len(machines) == 0 {
		return "", errors.New("no terminal-capable machines are available; run `pb setup` or `pb machine add`")
	}
	choices := make([]string, 0, len(machines))
	for _, machine := range machines {
		choices = append(choices, fmt.Sprintf("%s (%s)", machine.Alias, machine.ID))
	}
	return "", fmt.Errorf("multiple environments are available: %s; choose one with `pb <environment>`", strings.Join(choices, ", "))
}

func defaultWorkspaceEnvironment(ctx context.Context, commandContext *command.Context, client *api.Client, cfg *config.Config) (string, error) {
	workspace := client.Workspace()
	if workspace == "" {
		var err error
		workspace, err = workspaceForCommandContext(commandContext)
		if err != nil {
			return "", err
		}
	}
	accountID, err := workspaceAccountID(cfg)
	if err != nil {
		return "", err
	}
	rememberedID := ""
	if accountID != "" {
		rememberedID, err = workspaceLastEnvironment(cfg, cfg.ServerURL, accountID, workspace)
		if err != nil {
			return "", err
		}
	}
	return defaultEnvironment(ctx, client, rememberedID, workspace)
}

func resolveEnvironmentTarget(ctx context.Context, client *api.Client, requested string) (environmentTarget, error) {
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, nil
}

func resolveTerminalEnvironmentTarget(ctx context.Context, client *api.Client, requested string) (environmentTarget, error) {
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, err
	}
	if err := terminalHostError(machine); err != nil {
		return environmentTarget{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, nil
}

func terminalHostMachines(machines []api.UserMachine) []api.UserMachine {
	eligible := make([]api.UserMachine, 0, len(machines))
	for _, machine := range machines {
		if terminalHostError(machine) == nil {
			eligible = append(eligible, machine)
		}
	}
	return eligible
}

func terminalHostError(machine api.UserMachine) error {
	if !machine.Capabilities.TerminalHost.Configured {
		return &api.APIError{Code: "machine_capability_unavailable", Message: "This machine is not configured to host terminals."}
	}
	if !machine.Online || !machine.Capabilities.TerminalHost.Observed {
		return &api.APIError{Code: "machine_offline", Message: "This terminal host is offline."}
	}
	return nil
}

func listTerminalSessionsForTarget(ctx context.Context, client *api.Client, target environmentTarget) ([]api.TerminalSession, error) {
	return client.ListUserMachineTerminalSessions(ctx, target.id)
}

func createTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, name, idempotencyKey string) (api.TerminalSession, error) {
	return client.CreateUserMachineTerminalSession(ctx, target.id, name, idempotencyKey)
}

func renameTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID, name string) (api.TerminalSession, error) {
	return client.RenameUserMachineTerminalSession(ctx, target.id, sessionID, name)
}

func closeTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID string) error {
	return client.CloseUserMachineTerminalSession(ctx, target.id, sessionID)
}

func deleteTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID string) error {
	return client.DeleteUserMachineTerminalSession(ctx, target.id, sessionID)
}

// deps bundles production dependencies for a command.
type deps struct {
	cfg              *config.Config
	auth             config.AuthSource
	resolver         resolver.MachineResolver
	tunnel           tunnel.Tunnel
	peerTunnel       *tunnel.PeerTerminalTunnel
	peerLocal        *localapi.Client
	peerApplications peerApplicationTunnel
	telemetry        telemetry.Sink
}

type peerApplicationTunnel interface {
	Dial(context.Context, resolver.ConnectInfo) (tunnel.Conn, error)
	DialExec(context.Context, resolver.ConnectInfo, tunnel.ExecRequest) (tunnel.ExecConn, error)
	DialSSH(context.Context, resolver.ConnectInfo, string) (tunnel.Conn, error)
}

func buildDeps(c *command.Context) (*deps, error) {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return nil, err
	}
	if s := c.String("server"); s != "" {
		normalized, err := config.NormalizeServerURL(s)
		if err != nil {
			return nil, err
		}
		cfg.ServerURL = normalized
	} else if cfg.ServerURL != "" {
		normalized, err := config.NormalizeServerURL(cfg.ServerURL)
		if err != nil {
			return nil, fmt.Errorf("invalid configured Paperboat server: %w", err)
		}
		cfg.ServerURL = normalized
	}
	var authSource config.AuthSource = config.NoCredentialsSource{}
	if cfg.ServerURL != "" {
		authSource, err = sessionauth.NewSource(cfg)
		if err != nil {
			return nil, err
		}
	}
	var selectedTunnel tunnel.Tunnel
	var peerTunnel *tunnel.PeerTerminalTunnel
	var peerApplications peerApplicationTunnel
	var peerLocal *localapi.Client
	if cfg.ServerURL != "" {
		store, storeErr := config.ProfileStoreFor(cfg)
		if storeErr != nil {
			return nil, storeErr
		}
		transportConfig := httptransport.DevelopmentConfig()
		if transportConfig.TLSConfig == nil {
			transportConfig.TLSConfig = &tls.Config{}
		}
		transportConfig.TLSConfig.MinVersion = tls.VersionTLS13
		peerTransport, transportErr := httptransport.New(transportConfig)
		if transportErr != nil {
			return nil, transportErr
		}
		var peerErr error
		peerTunnel, peerErr = tunnel.NewPeerTerminalTunnel(tunnel.PeerTerminalConfig{Issuer: cfg.ServerURL, Store: store, Auth: authSource, TLS: transportConfig.TLSConfig, HTTPClient: &http.Client{Transport: peerTransport}, OutputQueueChunks: cfg.Connect.TerminalOutputQueueChunks})
		if peerErr != nil {
			return nil, peerErr
		}
		paths, pathsErr := currentLocalDaemonPaths()
		if pathsErr != nil {
			return nil, pathsErr
		}
		localClient, clientErr := localapi.NewClient(paths.SocketPath, time.Duration(config.PeerConnectTimeoutMilliseconds)*time.Millisecond)
		if clientErr != nil {
			return nil, clientErr
		}
		localPeer := &tunnel.LocalPeerTunnel{Client: localClient}
		peerApplications = localPeer
		peerLocal = localClient
		selectedTunnel = localPeer
	}
	return &deps{
		cfg:              cfg,
		auth:             authSource,
		resolver:         nil,
		tunnel:           selectedTunnel,
		peerTunnel:       peerTunnel,
		peerLocal:        peerLocal,
		peerApplications: peerApplications,
	}, nil
}

type pingSample struct {
	Sequence     int     `json:"sequence"`
	Path         string  `json:"path,omitempty"`
	ConnectionMS float64 `json:"connection_ms,omitempty"`
	Lost         bool    `json:"lost"`
	Transition   bool    `json:"path_transition"`
}

type pingReport struct {
	Schema              string       `json:"schema"`
	MachineID           string       `json:"machine_id"`
	MachineName         string       `json:"machine_name"`
	Sent                int          `json:"sent"`
	Received            int          `json:"received"`
	Lost                int          `json:"lost"`
	LossPercent         float64      `json:"loss_percent"`
	MinConnectionMS     float64      `json:"min_connection_ms,omitempty"`
	AverageConnectionMS float64      `json:"average_connection_ms,omitempty"`
	MaxConnectionMS     float64      `json:"max_connection_ms,omitempty"`
	Samples             []pingSample `json:"samples"`
}

// peerProber is the existing authenticated daemon probe boundary.
type peerProber interface {
	ProbePeer(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)
}

func probeDaemonPeer(ctx context.Context, client peerProber, target resolver.ConnectInfo, operation string) (tunnel.NativeProbe, error) {
	if client == nil || target.Terminal == nil {
		return tunnel.NativeProbe{}, errors.New("authenticated daemon probe is unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return tunnel.NativeProbe{}, errors.New("daemon probe requires a deadline")
	}
	request, err := localapi.NewPeerStreamRequest(target.MachineID, target.Terminal.EnvironmentID, target.MachineGeneration, "health_probe", operation, "local-health-probe", deadline, 1<<20, nil)
	if err != nil {
		return tunnel.NativeProbe{}, err
	}
	probe, err := client.ProbePeer(ctx, request)
	if err != nil {
		return tunnel.NativeProbe{}, err
	}
	return tunnel.NativeProbe{Path: probe.Path, Connection: time.Duration(probe.ConnectionNanoseconds)}, nil
}

func actionPing(command *cobra.Command, args []string) error {
	count, _ := command.Flags().GetInt("count")
	timeout, _ := command.Flags().GetDuration("timeout")
	jsonOutput, _ := command.Flags().GetBool("json")
	if count < 1 || count > 100 {
		return invocationError(errors.New("--count must be between 1 and 100"))
	}
	if timeout <= 0 || timeout > time.Minute {
		return invocationError(errors.New("--timeout must be greater than zero and no more than 1m"))
	}
	ctx := actionContext(command, args)
	dependencies, err := buildDeps(ctx)
	if err != nil {
		return err
	}
	if dependencies.peerLocal == nil {
		return errors.New("pb ping requires an authenticated Paperboat server")
	}
	client, err := backendClient(ctx)
	if err != nil {
		return err
	}
	machine, err := resolveUserMachine(command.Context(), client, args[0])
	if err != nil {
		return err
	}
	if !machine.Online {
		return &api.APIError{Code: "machine_offline", Message: "This machine is offline."}
	}
	if err := requireLocalDaemonService(command.Context(), dependencies.cfg); err != nil {
		return fmt.Errorf("prepare local peer transport: %w", err)
	}
	target := resolver.ConnectInfo{TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
	report := pingReport{Schema: "paperboat.ping/v1", MachineID: machine.ID, MachineName: machine.Alias, Sent: count, Samples: make([]pingSample, 0, count)}
	previousSelection := ""
	var total time.Duration
	for sequence := 1; sequence <= count; sequence++ {
		sampleCtx, cancel := context.WithTimeout(command.Context(), timeout)
		result, pingErr := probeDaemonPeer(sampleCtx, dependencies.peerLocal, target, newIdempotencyKey())
		cancel()
		if pingErr != nil {
			if command.Context().Err() != nil {
				return command.Context().Err()
			}
			if !errors.Is(pingErr, context.DeadlineExceeded) && !errors.Is(pingErr, context.Canceled) {
				return fmt.Errorf("authenticated ping sample %d: %w", sequence, pingErr)
			}
			report.Lost++
			report.Samples = append(report.Samples, pingSample{Sequence: sequence, Lost: true})
			if !jsonOutput {
				fmt.Fprintf(command.OutOrStdout(), "sample %d: timeout\n", sequence)
			}
			continue
		}
		path := pingPath(result.Path)
		selection := path
		transition := previousSelection != "" && previousSelection != selection
		previousSelection = selection
		connectionMS := float64(result.Connection) / float64(time.Millisecond)
		report.Received++
		total += result.Connection
		if report.MinConnectionMS == 0 || connectionMS < report.MinConnectionMS {
			report.MinConnectionMS = connectionMS
		}
		if connectionMS > report.MaxConnectionMS {
			report.MaxConnectionMS = connectionMS
		}
		sample := pingSample{Sequence: sequence, Path: path, ConnectionMS: connectionMS, Transition: transition}
		report.Samples = append(report.Samples, sample)
		if !jsonOutput {
			transitionText := ""
			if transition {
				transitionText = " transition"
			}
			fmt.Fprintf(command.OutOrStdout(), "sample %d: path=%s connect=%.2fms%s\n", sequence, path, sample.ConnectionMS, transitionText)
		}
	}
	if report.Received > 0 {
		report.AverageConnectionMS = float64(total) / float64(time.Millisecond) / float64(report.Received)
	}
	report.LossPercent = float64(report.Lost) * 100 / float64(report.Sent)
	if jsonOutput {
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}
	fmt.Fprintf(command.OutOrStdout(), "summary: sent=%d received=%d loss=%.1f%%", report.Sent, report.Received, report.LossPercent)
	if report.Received > 0 {
		fmt.Fprintf(command.OutOrStdout(), " connect min/avg/max=%.2f/%.2f/%.2fms", report.MinConnectionMS, report.AverageConnectionMS, report.MaxConnectionMS)
	}
	fmt.Fprintln(command.OutOrStdout())
	if report.Received == 0 {
		return exitCodeError{code: 1}
	}
	return nil
}

func pingPath(path string) string {
	switch path {
	case "direct", "peer_relay", "regional_relay", "unknown":
		return path
	default:
		return "unknown"
	}
}

func nativeSnapshotPath(path string) string {
	switch path {
	case "direct":
		return "direct"
	case "peer_relay", "regional_relay":
		return "relay"
	default:
		return ""
	}
}

func environmentsCommand() *command.Spec {
	return &command.Spec{
		Name:  "environments",
		Usage: "List enrolled machines available to this account",
		Flags: []command.Flag{&command.BoolFlag{Name: "json"}},
		Action: func(c *command.Context) error {
			client, err := backendClient(c)
			if err != nil {
				return err
			}
			machines, err := client.ListUserMachinesFiltered(c.Context, commandInventoryFilters(c))
			if err != nil {
				return err
			}
			if c.Bool("json") {
				return json.NewEncoder(c.Writer).Encode(map[string]any{"machines": machines})
			}
			w := tabwriter.NewWriter(c.Writer, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tPLATFORM\tID")
			for _, machine := range machines {
				state := machine.State
				if machine.Online && state == "" {
					state = "online"
				}
				platform := strings.Trim(strings.Join([]string{machine.Platform, machine.Architecture}, "/"), "/")
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", machine.Alias, state, platform, machine.ID)
			}
			return w.Flush()
		},
	}
}

var errOwnerOnlyFileInvalid = errors.New("invalid owner-only file")

func readOwnerOnlyFile(path string, limit int64) (data []byte, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateOwnerOnlyRegularFile(path, info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			clear(data)
			data = nil
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errOwnerOnlyFileInvalid
	}
	if err := validateOwnerOnlyRegularFile(path, opened); err != nil {
		return nil, err
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		clear(data)
		return nil, err
	}
	if int64(len(data)) > limit {
		clear(data)
		return nil, errOwnerOnlyFileInvalid
	}
	return data, nil
}

func inboxCommand() *command.Spec {
	return &command.Spec{Name: "inbox", Usage: "Manage the Paperboat Inbox", Subcommands: []*command.Spec{
		{Name: "path", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: inboxPathCommand},
		{Name: "set", ArgsUsage: "<directory>", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: inboxSetCommand},
		{Name: "reset", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: inboxResetCommand},
		{Name: "requests", Usage: "List team file requests", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: inboxRequestsCommand},
		{Name: "approve", ArgsUsage: "<request-id>", Usage: "Approve an exact team file request", Flags: []command.Flag{&command.UintFlag{Name: "generation"}, &command.BoolFlag{Name: "json"}}, Action: func(c *command.Context) error { return inboxDecisionCommand(c, "approve") }},
		{Name: "decline", ArgsUsage: "<request-id>", Usage: "Decline an exact team file request", Flags: []command.Flag{&command.UintFlag{Name: "generation"}, &command.BoolFlag{Name: "json"}}, Action: func(c *command.Context) error { return inboxDecisionCommand(c, "decline") }},
		{Name: "policy", ArgsUsage: "[manual|automatic]", Usage: "Show or update team file acceptance", Flags: []command.Flag{&command.BoolFlag{Name: "receipt-email"}, &command.BoolFlag{Name: "json"}}, Action: inboxPolicyCommand},
	}}
}

func inboxRequestsCommand(c *command.Context) error {
	if c.Args().Len() != 0 {
		return localArgumentError("pb inbox requests does not accept arguments")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	requests, err := client.TeamInboxRequests(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]any{"requests": requests}})
	}
	if len(requests) == 0 {
		fmt.Fprintln(c.Writer, "No team file requests.")
		return nil
	}
	w := tabwriter.NewWriter(c.Writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REQUEST\tSENDER\tFILES\tSTATUS\tEXPIRES")
	for _, request := range requests {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", request.RequestID, request.SenderAccount, len(request.Files), request.Status, relativeTimestamp(request.ExpiresAt))
	}
	return w.Flush()
}

func inboxDecisionCommand(c *command.Context, action string) error {
	if c.Args().Len() != 1 {
		return localArgumentError("usage: pb inbox approve|decline <request-id> --generation <generation>")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	generation := uint64(c.Uint("generation"))
	if generation == 0 {
		request, getErr := client.TeamInboxRequest(c.Context, c.Args().First())
		if getErr != nil {
			return friendlyCommandError(getErr)
		}
		generation = request.DecisionGeneration
	}
	request, err := client.DecideTeamInboxRequest(c.Context, c.Args().First(), action, generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": request})
	}
	fmt.Fprintf(c.Writer, "%s: %s\n", request.RequestID, request.Status)
	return nil
}

func inboxPolicyCommand(c *command.Context) error {
	if c.Args().Len() > 1 {
		return localArgumentError("usage: pb inbox policy [manual|automatic] [--receipt-email]")
	}
	if c.Args().Len() == 1 && c.Args().First() != "manual" && c.Args().First() != "automatic" {
		return localArgumentError("acceptance must be manual or automatic")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	policy, err := client.TeamInboxPolicy(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Args().Len() == 1 {
		acceptance := c.Args().First()
		policy.Acceptance, policy.ReceiptEmail = acceptance, c.Bool("receipt-email")
		policy, err = client.SetTeamInboxPolicy(c.Context, policy)
		if err != nil {
			return friendlyCommandError(err)
		}
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": policy})
	}
	fmt.Fprintf(c.Writer, "Acceptance: %s\nReceipt email: %t\n", policy.Acceptance, policy.ReceiptEmail)
	return nil
}

func runtimeIdentityStore() (*identity.Store, error) {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return nil, err
		}
	}
	return identity.Open(identity.Config{StateRoot: stateRoot})
}

func configuredInboxPath() (string, error) {
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if err != nil {
		return "", commandPreparationFailure{step: prepareInboxRegistration, cause: err}
	}
	if err := inbox.ValidatePath(registration.InboxPath); err != nil {
		return "", err
	}
	return registration.InboxPath, nil
}

func configuredMachineID() (string, error) {
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID == "" {
		return "", commandPreparationFailure{step: prepareMachineRegistration, cause: err}
	}
	return registration.MachineID, nil
}

func inboxPathCommand(c *command.Context) error {
	path, err := configuredInboxPath()
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]string{"path": path}})
	}
	fmt.Fprintln(c.Writer, path)
	return nil
}

func inboxSetCommand(c *command.Context) error {
	if c.Args().Len() != 1 {
		return localArgumentError("usage: pb inbox set <directory>")
	}
	return setInboxPath(c, c.Args().First())
}

func inboxResetCommand(c *command.Context) error {
	if c.Args().Len() != 0 {
		return localArgumentError("pb inbox reset does not accept arguments")
	}
	path, err := inbox.DefaultPath()
	if err != nil {
		return err
	}
	return setInboxPath(c, path)
}

func setInboxPath(c *command.Context, path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if err := inbox.EnsurePath(path); err != nil {
		return err
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil {
		return commandPreparationFailure{step: prepareInboxRegistration, cause: err}
	}
	registration.InboxPath = path
	registration.UpdatedAt = time.Now().UTC()
	if err := store.SaveRegistration(registration); err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]string{"path": path}})
	}
	fmt.Fprintln(c.Writer, path)
	return nil
}

func sessionsCommand() *command.Spec {
	list := func(c *command.Context) error {
		client, err := backendClient(c)
		if err != nil {
			return err
		}
		requested := strings.TrimSpace(c.Args().First())
		if requested == "" {
			cfg, loadErr := workspaceConfig(c.String("config"), c.String("server"))
			if loadErr != nil {
				return loadErr
			}
			requested, err = defaultWorkspaceEnvironment(c.Context, c, client, cfg)
			if err != nil {
				return err
			}
		}
		target, err := resolveTerminalEnvironmentTarget(c.Context, client, requested)
		if err != nil {
			return err
		}
		sessions, err := client.ListUserMachineTerminalSessionsFiltered(c.Context, target.id, commandInventoryFilters(c))
		if err != nil {
			return friendlyCommandError(err)
		}
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "sessions": sessions})
		}
		w := tabwriter.NewWriter(c.Writer, 0, 4, 2, ' ', 0)
		if c.Bool("wide") {
			fmt.Fprintln(w, "NAME\tID\tTITLE / PROCESS\tDIRECTORY\tSTATE\tATTACHED\tLAST ACTIVE\tCREATED")
		} else {
			fmt.Fprintln(w, "NAME\tTITLE / PROCESS\tDIRECTORY\tSTATE\tATTACHED\tLAST ACTIVE\tCREATED")
		}
		for _, s := range sessions {
			attached := "-"
			if s.AttachedCount != nil {
				attached = fmt.Sprintf("%d", *s.AttachedCount)
			}
			if c.Bool("wide") {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", selector.SanitizeText(s.Name), s.ID, sessionActivityLabel(s), sessionDirectoryLabel(s), s.State, attached, relativeTime(s.LastActiveAt), relativeTimestamp(s.CreatedAt))
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", selector.SanitizeText(s.Name), sessionActivityLabel(s), sessionDirectoryLabel(s), s.State, attached, relativeTime(s.LastActiveAt), relativeTimestamp(s.CreatedAt))
			}
		}
		return w.Flush()
	}
	return &command.Spec{Name: "session", Usage: "Manage environment terminal sessions", ArgsUsage: "<environment>", Flags: []command.Flag{&command.BoolFlag{Name: "wide"}, &command.BoolFlag{Name: "json"}}, Action: list, Subcommands: []*command.Spec{
		{Name: "rename", ArgsUsage: "<environment> <session> <new-name>", Usage: "Rename a terminal session", Flags: []command.Flag{&command.BoolFlag{Name: "json", Usage: "emit JSON"}}, Action: func(c *command.Context) error {
			if c.Args().Len() != 3 {
				return localArgumentError("usage: pb session rename <environment> <session> <new-name>")
			}
			if err := validateSessionName(c.Args().Get(2)); err != nil {
				return err
			}
			client, err := backendClient(c)
			if err != nil {
				return err
			}
			target, err := resolveTerminalEnvironmentTarget(c.Context, client, c.Args().First())
			if err != nil {
				return err
			}
			session, err := resolveTerminalSession(c.Context, client, target, c.Args().Get(1))
			if err != nil {
				return err
			}
			if session.IsDefault {
				return commandRejection{reason: commandRejectDefaultSessionRename}
			}
			updated, err := renameTerminalSessionForTarget(c.Context, client, target, session.ID, c.Args().Get(2))
			if err != nil {
				return friendlyCommandError(err)
			}
			if c.Bool("json") {
				return writeCLIJSON(c.Writer, map[string]any{"environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "session": updated})
			}
			fmt.Fprintf(c.Writer, "Renamed session %s to %s.\n", session.Name, updated.Name)
			return nil
		}},
		{Name: "close", ArgsUsage: "<environment> [<session>]", Usage: "Close one or all terminal sessions", Action: func(c *command.Context) error {
			all := c.Bool("all")
			if c.Args().Len() < 1 || c.Args().Len() > 2 || all && c.Args().Len() != 1 {
				return localArgumentError("usage: pb session close <environment> [<session>] [--all] [--confirm CODE]")
			}
			client, err := backendClient(c)
			if err != nil {
				return err
			}
			target, err := resolveTerminalEnvironmentTarget(c.Context, client, c.Args().First())
			if err != nil {
				return err
			}
			if all {
				sessions, err := listTerminalSessionsForTarget(c.Context, client, target)
				if err != nil {
					return friendlyCommandError(err)
				}
				open := make([]api.TerminalSession, 0, len(sessions))
				for _, session := range sessions {
					if session.State != "closed" {
						open = append(open, session)
					}
				}
				if !c.Bool("json") {
					fmt.Fprintf(c.ErrWriter, "Environment: %s (%s)\n", target.name, target.id)
					fmt.Fprintf(c.ErrWriter, "Open sessions to close: %d\n", len(open))
				}
				ids := make([]string, 0, len(open))
				for _, session := range open {
					ids = append(ids, session.ID)
				}
				if len(open) == 0 {
					if c.Bool("json") {
						return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "closed": 0})
					}
					fmt.Fprintln(c.Writer, "No open sessions to close.")
					return nil
				}
				if err := confirmContextMutation(c, "session-close-all:"+target.id+":"+sortedSessionScope(ids), fmt.Sprintf("Close %d open sessions in %s (%s)? Their remote processes will end and recent output will be deleted.", len(open), target.name, target.id)); err != nil {
					return err
				}
				var closeErrors []error
				closed := 0
				for _, session := range open {
					if err := closeTerminalSessionForTarget(c.Context, client, target, session.ID); err != nil {
						closeErrors = append(closeErrors, fmt.Errorf("close session %s: %w", session.Name, err))
						continue
					}
					closed++
				}
				if len(closeErrors) > 0 {
					return fmt.Errorf("closed %d sessions in %s; remote state changed; rerun without --confirm to preview the remaining sessions: %w", closed, target.name, errors.Join(closeErrors...))
				}
				if c.Bool("json") {
					return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "closed": closed})
				}
				fmt.Fprintf(c.Writer, "Closed %d sessions in %s. Recent output was deleted.\n", closed, target.name)
				return nil
			}
			var session api.TerminalSession
			if c.Args().Len() == 1 {
				sessions, listErr := listTerminalSessionsForTarget(c.Context, client, target)
				if listErr != nil {
					return friendlyCommandError(listErr)
				}
				session, err = selectSession(c.Context, client, target, slices.DeleteFunc(sessions, func(item api.TerminalSession) bool { return item.State == "closed" }), "Choose a session to close")
			} else {
				session, err = resolveTerminalSession(c.Context, client, target, c.Args().Get(1))
			}
			if err != nil {
				return err
			}
			if err := confirmContextMutationWithArgs(c, "session-close:"+target.id+":"+session.ID, fmt.Sprintf("Close terminal session %q in %s (%s)? Its process will end and recent output will be deleted.", session.Name, target.name, target.id), []string{c.Args().First(), session.ID}); err != nil {
				return err
			}
			if err := closeTerminalSessionForTarget(c.Context, client, target, session.ID); err != nil {
				return friendlyCommandError(err)
			}
			if c.Bool("json") {
				return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "session_id": session.ID, "state": "closed"})
			}
			return nil
		}, Flags: []command.Flag{&command.StringFlag{Name: "confirm", Usage: "six-character confirmation code from the preview"}, &command.BoolFlag{Name: "all", Usage: "close all sessions in the environment"}, &command.BoolFlag{Name: "json", Usage: "emit JSON"}}},
		{Name: "delete", ArgsUsage: "<environment> [<session>]", Usage: "Delete a closed terminal session record", Flags: []command.Flag{&command.StringFlag{Name: "confirm", Usage: "six-character confirmation code from the preview"}, &command.BoolFlag{Name: "all", Usage: "delete all closed non-default sessions in the environment"}, &command.BoolFlag{Name: "json", Usage: "emit JSON"}}, Action: func(c *command.Context) error {
			all := c.Bool("all")
			if c.Args().Len() < 1 || c.Args().Len() > 2 || all && c.Args().Len() != 1 {
				return localArgumentError("usage: pb session delete <environment> [<session>] [--all] [--confirm CODE]")
			}
			client, err := backendClient(c)
			if err != nil {
				return err
			}
			target, err := resolveTerminalEnvironmentTarget(c.Context, client, c.Args().First())
			if err != nil {
				return err
			}
			if all {
				sessions, err := listTerminalSessionsForTarget(c.Context, client, target)
				if err != nil {
					return friendlyCommandError(err)
				}
				selected := slices.DeleteFunc(sessions, func(item api.TerminalSession) bool { return item.IsDefault || item.State != "closed" })
				if !c.Bool("json") {
					fmt.Fprintf(c.ErrWriter, "Environment: %s (%s)\n", target.name, target.id)
					fmt.Fprintf(c.ErrWriter, "Closed non-default sessions to delete: %d\n", len(selected))
				}
				ids := make([]string, 0, len(selected))
				for _, session := range selected {
					ids = append(ids, session.ID)
				}
				if len(selected) == 0 {
					if c.Bool("json") {
						return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "deleted": 0})
					}
					fmt.Fprintln(c.Writer, "No closed non-default sessions to delete.")
					return nil
				}
				if err := confirmContextMutation(c, "session-delete-all:"+target.id+":"+sortedSessionScope(ids), fmt.Sprintf("Delete %d closed non-default sessions in %s (%s)? Their records and retained history will be removed. Open and default sessions are excluded.", len(selected), target.name, target.id)); err != nil {
					return err
				}
				var deleteErrors []error
				deleted := 0
				for _, session := range selected {
					if err := deleteTerminalSessionForTarget(c.Context, client, target, session.ID); err != nil {
						deleteErrors = append(deleteErrors, fmt.Errorf("delete session %s: %w", session.Name, err))
						continue
					}
					deleted++
				}
				if len(deleteErrors) > 0 {
					return fmt.Errorf("deleted %d of %d sessions in %s; remote state changed; rerun without --confirm to preview the remaining sessions: %w", deleted, len(selected), target.name, errors.Join(deleteErrors...))
				}
				if c.Bool("json") {
					return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "deleted": deleted})
				}
				fmt.Fprintf(c.Writer, "Deleted %d closed session records in %s.\n", deleted, target.name)
				return nil
			}
			var session api.TerminalSession
			if c.Args().Len() == 1 {
				sessions, listErr := listTerminalSessionsForTarget(c.Context, client, target)
				if listErr != nil {
					return friendlyCommandError(listErr)
				}
				session, err = selectSession(c.Context, client, target, slices.DeleteFunc(sessions, func(item api.TerminalSession) bool { return item.IsDefault || item.State != "closed" }), "Choose a closed session to delete")
			} else {
				session, err = resolveTerminalSession(c.Context, client, target, c.Args().Get(1))
			}
			if err != nil {
				return err
			}
			if session.IsDefault {
				return commandRejection{reason: commandRejectDefaultSessionDelete}
			}
			if session.State != "closed" {
				return commandRejection{reason: commandRejectOpenSessionDelete}
			}
			if err := confirmContextMutationWithArgs(c, "session-delete:"+target.id+":"+session.ID, fmt.Sprintf("Delete closed terminal session %q in %s (%s)? Its record and retained history will be removed.", session.Name, target.name, target.id), []string{c.Args().First(), session.ID}); err != nil {
				return err
			}
			if err := deleteTerminalSessionForTarget(c.Context, client, target, session.ID); err != nil {
				return friendlyCommandError(err)
			}
			if c.Bool("json") {
				return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "session_id": session.ID, "deleted": true})
			}
			return nil
		}},
	}}
}

func selectTerminalSession(ctx context.Context, client *api.Client, projectRef, name, ref string) (api.TerminalSession, environmentTarget, api.UserMachine, error) {
	target, machine, err := resolveEnvironmentTargetWithMachine(ctx, client, projectRef)
	if err != nil {
		return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, err
	}
	if strings.TrimSpace(ref) == "" {
		if err := validateSessionNameOptional(name); err != nil {
			return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, err
		}
		session, err := createTerminalSessionForTarget(ctx, client, target, name, newIdempotencyKey())
		if err != nil {
			return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, friendlyCommandError(err)
		}
		if session.EvictedSession != nil {
			fmt.Fprintf(os.Stderr, "Session limit reached; removed least-recent session %q (%s).\n", session.EvictedSession.Name, session.EvictedSession.State)
		}
		return session, target, machine, nil
	}
	session, err := resolveTerminalSession(ctx, client, target, ref)
	if err != nil {
		return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, friendlyCommandError(err)
	}
	return session, target, machine, nil
}

// resolveEnvironmentTargetWithMachine resolves like resolveEnvironmentTarget but
// also returns the machine catalog entry for machine targets so callers can
// reuse it without a second listing round trip. The daemon's warm inventory
// snapshot is consulted first: a machine it already tracks needs no catalog
// listing at all, and the server revalidates the machine on every session
// create and connection descriptor anyway.
func resolveEnvironmentTargetWithMachine(ctx context.Context, client *api.Client, requested string) (environmentTarget, api.UserMachine, error) {
	if machine, ok := resolveWarmUserMachine(ctx, requested); ok && machine.InstallationGeneration > 0 {
		return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, machine, nil
	}
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, api.UserMachine{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, machine, nil
}

func resolveUserMachine(ctx context.Context, client *api.Client, requested string) (api.UserMachine, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return api.UserMachine{}, err
	}
	for _, machine := range machines {
		if machine.ID == requested {
			return machine, nil
		}
	}
	var matches []api.UserMachine
	for _, machine := range machines {
		if strings.EqualFold(machine.Alias, requested) {
			matches = append(matches, machine)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, machine := range matches {
			ids = append(ids, machine.ID)
		}
		return api.UserMachine{}, fmt.Errorf("%w: %q matches machine IDs %s; use an exact ID", resolver.ErrMachineAmbiguous, requested, strings.Join(ids, ", "))
	}
	return api.UserMachine{}, fmt.Errorf("%w: %q", resolver.ErrMachineNotFound, requested)
}

// resolveWarmUserMachine uses the daemon's authenticated inventory snapshot
// for selection. Operation descriptors remain fetched from the control plane;
// this only removes a redundant catalog round trip on an already-warm client.
var loadWarmMachineSnapshot = func(ctx context.Context) (localapi.Snapshot, error) {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 300*time.Millisecond)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	return client.Snapshot(ctx)
}

func resolveWarmUserMachine(ctx context.Context, requested string) (api.UserMachine, bool) {
	if _, scoped := workspaceFromContext(ctx); scoped {
		return api.UserMachine{}, false
	}
	snapshot, err := loadWarmMachineSnapshot(ctx)
	if err != nil {
		return api.UserMachine{}, false
	}
	status, err := localwait.ResolveMachine(snapshot.Machines, requested)
	if err != nil || !status.Eligible || status.Generation == 0 {
		return api.UserMachine{}, false
	}
	return api.UserMachine{ID: status.ID, EnvironmentID: status.EnvironmentID, WorkspaceRoot: status.WorkspaceRoot, Alias: status.Alias, Platform: status.Platform, State: "ready", Online: status.RuntimeState == "ready", InstallationGeneration: int64(status.Generation)}, true
}

func resolveTerminalSession(ctx context.Context, client *api.Client, target environmentTarget, ref string) (api.TerminalSession, error) {
	sessions, err := listTerminalSessionsForTarget(ctx, client, target)
	if err != nil {
		return api.TerminalSession{}, friendlyCommandError(err)
	}
	for _, s := range sessions {
		if s.ID == ref || strings.EqualFold(s.Name, ref) {
			return s, nil
		}
	}
	var suggestions []string
	for _, s := range sessions {
		if strings.HasPrefix(strings.ToLower(s.Name), strings.ToLower(ref)) || editDistance(strings.ToLower(s.Name), strings.ToLower(ref)) <= 2 {
			suggestions = append(suggestions, s.Name)
			if len(suggestions) == 3 {
				break
			}
		}
	}
	message := fmt.Sprintf("terminal session %q was not found", ref)
	if len(suggestions) > 0 {
		message += "; did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	message += "; create one with `pb <environment> new`"
	return api.TerminalSession{}, errors.New(message)
}

var sessionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var automaticSessionNamePattern = regexp.MustCompile(`^shell-[0-9]+$`)

func validateSessionNameOptional(name string) error {
	if name == "" {
		return nil
	}
	return validateSessionName(name)
}
func validateSessionName(name string) error {
	if name == "default" || automaticSessionNamePattern.MatchString(name) || !sessionNamePattern.MatchString(name) {
		return localArgumentError("session names must be lowercase 1-64 character values matching [a-z0-9][a-z0-9._-]{0,63}; default and shell-N are reserved")
	}
	return nil
}

func newIdempotencyKey() string {
	return "operation_" + uuid.NewString()
}
func relativeTime(at *time.Time) string {
	if at == nil {
		return "-"
	}
	d := time.Since(*at)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ra := range a {
		current := make([]int, len(b)+1)
		current[0] = i + 1
		for j, rb := range b {
			cost := 0
			if ra != rb {
				cost = 1
			}
			current[j+1] = minInt(current[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = current
	}
	return prev[len(b)]
}
func minInt(values ...int) int {
	value := values[0]
	for _, candidate := range values[1:] {
		if candidate < value {
			value = candidate
		}
	}
	return value
}

// Presentation belongs to the invocation owner; forwarding keeps typed
// status, attempt ownership and support references intact.
func friendlyCommandError(err error) error { return err }

func actionConnect(c *command.Context) error {
	return actionConnectTarget(c, "")
}

func actionExecCobra(cobraCommand *cobra.Command, args []string, shorthand bool) error {
	dash := cobraCommand.ArgsLenAtDash()
	if dash < 0 || dash >= len(args) || dash != 1 || len(args[dash:]) == 0 {
		return invocationError(errors.New("remote execution requires <machine> -- <argv...>"))
	}
	request := tunnel.ExecRequest{Argv: append([]string(nil), args[dash:]...)}
	jsonOutput := false
	if !shorthand {
		request.CWD, _ = cobraCommand.Flags().GetString("cwd")
		request.Timeout, _ = cobraCommand.Flags().GetDuration("timeout")
		request.PTY, _ = cobraCommand.Flags().GetBool("pty")
		jsonOutput, _ = cobraCommand.Flags().GetBool("json")
		values, _ := cobraCommand.Flags().GetStringArray("env")
		var err error
		request.Environment, err = parseExecEnvironment(values)
		if err != nil {
			return invocationError(err)
		}
	}
	return actionRemoteExec(actionContext(cobraCommand, args), args[0], request, jsonOutput)
}

func actionSSH(command *cobra.Command, args []string) error {
	dash := command.ArgsLenAtDash()
	if len(args) == 0 || dash > 1 || dash < 0 && len(args) != 1 {
		return invocationError(errors.New("SSH requires [user@]<machine> [-- <OpenSSH arguments...>]"))
	}
	targetName, targetUser, err := managedssh.ParseMachineTarget(args[0])
	if err != nil {
		return invocationError(err)
	}
	flagUser, _ := command.Flags().GetString("user")
	requestedUser, err := resolveSSHRequestedUser(targetUser, flagUser)
	if err != nil {
		return invocationError(err)
	}
	ctx := actionContext(command, args)
	client, machine, target, err := resolveSSHCommandTargetFast(ctx, targetName)
	if err != nil {
		return friendlyCommandError(err)
	}
	_ = client
	destination, err := managedssh.ResolveDestination(managedssh.DestinationInput{Alias: machine.Alias, RegisteredPort: target.Port, RequestedUser: requestedUser, RegisteredUser: target.OSUser, HasRegisteredUser: true, Platform: machine.Platform})
	if err != nil {
		return err
	}
	environment := os.Environ()
	return executeManagedSSH(command, ctx, machine, destination, args[1:], dash == 1, environment)
}

func resolveSSHRequestedUser(targetUser, flagUser string) (string, error) {
	if flagUser != "" {
		if err := managedssh.ValidateUsername(flagUser); err != nil {
			return "", err
		}
	}
	if flagUser != "" && targetUser != "" && flagUser != targetUser {
		return "", managedssh.ErrSSHUsernameConflict
	}
	if flagUser != "" {
		return flagUser, nil
	}
	return targetUser, nil
}

func openSSHArguments(destination managedssh.Destination, passthrough []string, includePassthrough bool) []string {
	arguments := openSSHSecurityArguments()
	if destination.Port != 22 {
		arguments = append(arguments, "-p", strconv.Itoa(int(destination.Port)))
	}
	arguments = append(arguments, destination.User+"@"+destination.Host)
	if includePassthrough {
		arguments = append(arguments, passthrough...)
	}
	return arguments
}

func openSSHSecurityArguments() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "PreferredAuthentications=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
	}
}

func actionSSHProxy(command *cobra.Command, _ []string) error {
	if err := processlifetime.ArmParentDeath(command.Context()); err != nil {
		return err
	}
	host, _ := command.Flags().GetString("host")
	portText, _ := command.Flags().GetString("port")
	proxyUser, _ := command.Flags().GetString("user")
	if err := managedssh.ValidateUsername(proxyUser); err != nil {
		return err
	}
	alias, err := managedssh.ParseAliasHost(host)
	if err != nil {
		return err
	}
	ctx := actionContext(command, nil)
	_, machine, target, err := resolveSSHCommandTargetLive(ctx, alias)
	if err != nil {
		return friendlyCommandError(err)
	}
	if _, err := managedssh.ResolveDestination(managedssh.DestinationInput{Alias: machine.Alias, RegisteredPort: target.Port, RequestedUser: proxyUser, RegisteredUser: target.OSUser, HasRegisteredUser: true, Platform: machine.Platform}); err != nil {
		return err
	}
	if _, err := managedssh.ValidateDestinationPort(portText, target.Port); err != nil {
		return err
	}
	d, err := buildDeps(ctx)
	if err != nil {
		return err
	}
	if d.peerApplications == nil {
		return errors.New("Paperboat peer transport is unavailable")
	}
	operationID := newSSHOperationID()
	descriptor := pendingSSHDescriptor(machine, operationID)
	connection, err := d.peerApplications.DialSSH(command.Context(), sshConnectInfo(machine, descriptor), operationID)
	if err != nil {
		return err
	}
	halfCloser, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		_ = connection.Close()
		return tunnel.ErrInputEOFUnsupported
	}
	defer connection.Close()
	go func() {
		<-command.Context().Done()
		_ = connection.Close()
	}()
	err = copySSHProxy(connection, halfCloser, command.InOrStdin(), command.OutOrStdout())
	return err
}

func copySSHProxy(connection io.ReadWriteCloser, halfCloser interface{ CloseWrite() error }, input io.Reader, output io.Writer) error {
	type inputResult struct {
		copyErr       error
		closeWriteErr error
	}
	inputDone := make(chan inputResult, 1)
	go func() {
		_, copyErr := io.Copy(connection, input)
		inputDone <- inputResult{copyErr: copyErr, closeWriteErr: halfCloser.CloseWrite()}
	}()
	_, outputErr := io.Copy(output, connection)
	if outputErr != nil && !errors.Is(outputErr, io.EOF) {
		return outputErr
	}
	// Remote EOF terminates ProxyCommand even when OpenSSH deliberately keeps
	// its input pipe open until the proxy exits. Input EOF still half-closes the
	// stream above and permits all remaining remote output to drain first.
	select {
	case result := <-inputDone:
		// The authenticated remote EOF is authoritative. A concurrent local
		// half-close can observe any platform-specific socket shutdown error after
		// that EOF; this is normal full-duplex shutdown, not a failed SSH
		// transport. Input-copy failures remain authoritative because bytes may
		// have been lost before the remote EOF.
		if result.copyErr != nil {
			return result.copyErr
		}
		return nil
	case <-time.After(25 * time.Millisecond):
		// Keep remote EOF bounded when OpenSSH intentionally leaves stdin open,
		// while giving an already-failed input copy a deterministic chance to
		// publish its authoritative transport error.
		return nil
	}
}

func actionSSHKnownHosts(command *cobra.Command, _ []string) error {
	host, _ := command.Flags().GetString("host")
	portText, _ := command.Flags().GetString("port")
	alias, err := managedssh.ParseAliasHost(host)
	if err != nil {
		return err
	}
	client, machine, target, err := resolveSSHCommandTargetFast(actionContext(command, nil), alias)
	if err != nil {
		return friendlyCommandError(err)
	}
	if _, err := managedssh.ValidateDestinationPort(portText, target.Port); err != nil {
		return err
	}
	set, err := client.ManagedSSHHostKeys(command.Context(), machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return friendlyCommandError(err)
	}
	keys, err := managedssh.ParseHostPublicKeys(set.Keys)
	if err != nil {
		return err
	}
	knownHosts, err := managedssh.FormatKnownHosts(host, target.Port, keys)
	if err != nil {
		return err
	}
	_, err = command.OutOrStdout().Write(knownHosts)
	return err
}

func actionSSHTrustHost(command *cobra.Command, args []string) error {
	client, machine, _, err := resolveSSHCommandTarget(actionContext(command, args), args[0])
	if err != nil {
		return friendlyCommandError(err)
	}
	generation := uint64(machine.InstallationGeneration)
	active, err := client.ManagedSSHHostKeys(command.Context(), machine.ID, generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	pending, err := client.ManagedSSHPendingHostKeys(command.Context(), machine.ID, generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	fingerprint, _ := command.Flags().GetString("fingerprint")
	fingerprint = strings.TrimSpace(fingerprint)
	jsonOutput, _ := command.Flags().GetBool("json")
	if fingerprint != "" && fingerprint != pending.Fingerprint {
		return errors.New("pending SSH host fingerprint does not match --fingerprint")
	}
	if fingerprint == "" {
		if jsonOutput {
			return invocationError(errors.New("ssh trust-host with --json requires the exact --fingerprint"))
		}
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("interactive confirmation is required; pass the exact --fingerprint")
		}
		fmt.Fprintf(command.ErrOrStderr(), "Current SSH host: %s\nPending SSH host: %s\nTrust the pending host identity? [y/N] ", active.Fingerprint, pending.Fingerprint)
		answer, readErr := bufio.NewReader(command.InOrStdin()).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return errors.New("SSH host trust was not changed")
		}
		fingerprint = pending.Fingerprint
	}
	_, err = client.PromoteManagedSSHHostKeys(command.Context(), machine.ID, pending.SetID, fingerprint, newIdempotencyKey(), generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	if jsonOutput {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"machine": map[string]string{"id": machine.ID, "alias": machine.Alias}, "fingerprint": fingerprint, "trusted": true})
	}
	fmt.Fprintf(command.OutOrStdout(), "Trusted SSH host identity for %s (%s)\n", machine.Alias, fingerprint)
	return nil
}

func actionSSHDoctor(command *cobra.Command, args []string) error {
	capabilities, err := managedssh.ProbeOpenSSH(command.Context(), "ssh", 5*time.Second)
	if err != nil || !capabilities.Ready() {
		return errors.Join(managedssh.ErrOpenSSHUnavailable, err)
	}
	client, machine, target, err := resolveSSHCommandTarget(actionContext(command, args), args[0])
	if err != nil {
		return friendlyCommandError(err)
	}
	keys, err := client.ManagedSSHHostKeys(command.Context(), machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return friendlyCommandError(err)
	}
	cfg, store, err := requireAuthConfig(actionContext(command, args))
	if err != nil {
		return err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return err
	}
	identity, err := store.ManagedSSHIdentity(cfg.ServerURL, profile.CLIClientSessionID)
	if err != nil {
		return err
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	agentSocket := managedssh.InstalledAgentSocket(paths.RuntimeRoot)
	if err := managedssh.ValidateInstalledOpenSSHConfig(home, uint32(os.Geteuid()), agentSocket); err != nil {
		return fmt.Errorf("managed OpenSSH configuration is not ready; rerun `pb login`: %w", err)
	}
	if err := managedssh.ValidateManagedIdentityPublicKey(home, uint32(os.Geteuid()), identity.PublicKey); err != nil {
		return fmt.Errorf("managed SSH public identity is not ready; rerun `pb login`: %w", err)
	}
	if err := managedssh.ProbeAgentIdentity(command.Context(), agentSocket, identity.Fingerprint, 5*time.Second); err != nil {
		return fmt.Errorf("managed SSH agent is not ready; rerun `pb login`: %w", err)
	}
	d, err := buildDeps(actionContext(command, args))
	if err != nil {
		return err
	}
	if d.peerApplications == nil {
		return errors.New("Paperboat peer transport is unavailable")
	}
	operationID := newSSHOperationID()
	descriptor := pendingSSHDescriptor(machine, operationID)
	probeCtx, cancel := context.WithTimeout(command.Context(), 20*time.Second)
	defer cancel()
	connection, err := d.peerApplications.DialSSH(probeCtx, sshConnectInfo(machine, descriptor), operationID)
	if err != nil {
		return fmt.Errorf("managed SSH transport or loopback target is not ready: %w", err)
	}
	host, err := managedssh.AliasHost(machine.Alias)
	if err != nil {
		_ = connection.Close()
		return err
	}
	authErr := managedssh.ProbeSSHAuthentication(probeCtx, connection, net.JoinHostPort(host, strconv.Itoa(int(target.Port))), target.OSUser, identity.Signer, keys.Keys)
	closeErr := connection.Close()
	if authErr != nil {
		return fmt.Errorf("managed SSH host verification or key reconciliation is not ready: %w", authErr)
	}
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return fmt.Errorf("close managed SSH readiness probe: %w", closeErr)
	}
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{"openssh": map[string]any{"version": capabilities.Version, "ready": true}, "local_configuration": "ready", "managed_agent": "ready", "machine": map[string]any{"id": machine.ID, "alias": machine.Alias}, "transport": "ready", "target": map[string]any{"state": "ready", "port": target.Port}, "host_identity": keys.Fingerprint, "managed_keys": "reconciled"})
	}
	fmt.Fprintf(command.OutOrStdout(), "OpenSSH: %s\nLocal configuration: ready\nManaged agent: ready\nMachine: %s\nTransport: ready\nTarget: ready on port %d\nHost identity: %s\nManaged keys: reconciled\n", capabilities.Version, machine.Alias, target.Port, keys.Fingerprint)
	return nil
}

func resolveSSHCommandTarget(ctx *command.Context, requested string) (*api.Client, api.UserMachine, api.ManagedSSHTarget, error) {
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if d.cfg.ServerURL == "" {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("Paperboat server is required for SSH")
	}
	client, err := sshAPIClient(ctx, d)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	machine, err := resolveSSHMachine(ctx.Context, client, requested)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if machine.InstallationGeneration < 1 {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("machine has no active runtime generation")
	}
	target, err := client.ManagedSSHTarget(ctx.Context, machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	_ = sshTargetCacheStore(d.cfg, machine, target, time.Now())
	return client, machine, target, nil
}

// resolveSSHCommandTargetFast resolves the machine from the daemon's warm
// inventory snapshot and the SSH target from a short-lived local cache when
// fresh, avoiding catalog listing round trips on the interactive SSH path.
// Any unavailable shortcut falls back to the canonical live resolution so
// error behavior is unchanged.
func resolveSSHCommandTargetFast(ctx *command.Context, requested string) (*api.Client, api.UserMachine, api.ManagedSSHTarget, error) {
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if d.cfg.ServerURL == "" {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("Paperboat server is required for SSH")
	}
	machine, ok := resolveWarmUserMachine(ctx.Context, requested)
	if !ok || machine.InstallationGeneration < 1 {
		return resolveSSHCommandTarget(ctx, requested)
	}
	client, err := sshAPIClient(ctx, d)
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	if target, fresh := sshTargetCacheLookup(d.cfg, machine, time.Now()); fresh {
		return client, machine, target, nil
	}
	target, err := client.ManagedSSHTarget(ctx.Context, machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	_ = sshTargetCacheStore(d.cfg, machine, target, time.Now())
	return client, machine, target, nil
}

// resolveSSHCommandTargetLive skips the machine catalog listing through the
// daemon's warm snapshot but always validates the SSH target against fresh
// server data. The managed SSH proxy uses this so destination port validation
// can never trust a stale local cache.
func resolveSSHCommandTargetLive(ctx *command.Context, requested string) (*api.Client, api.UserMachine, api.ManagedSSHTarget, error) {
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, err
	}
	if d.cfg.ServerURL == "" {
		return nil, api.UserMachine{}, api.ManagedSSHTarget{}, errors.New("Paperboat server is required for SSH")
	}
	machine, ok := resolveWarmUserMachine(ctx.Context, requested)
	if !ok || machine.InstallationGeneration < 1 {
		return resolveSSHCommandTarget(ctx, requested)
	}
	client, err := sshAPIClient(ctx, d)
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	target, err := client.ManagedSSHTarget(ctx.Context, machine.ID, uint64(machine.InstallationGeneration))
	if err != nil {
		return resolveSSHCommandTarget(ctx, requested)
	}
	_ = sshTargetCacheStore(d.cfg, machine, target, time.Now())
	return client, machine, target, nil
}

func sshAPIClient(ctx *command.Context, d *deps) (*api.Client, error) {
	credential, err := d.auth.Credential()
	if err != nil {
		return nil, err
	}
	client, err := newWorkspaceAPIClient(ctx, d.cfg.ServerURL, credential)
	if err != nil {
		return nil, err
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return nil, err
	}
	client.SetSourceMachineID(sourceMachineID)
	return client, nil
}

func resolveSSHMachine(ctx context.Context, client *api.Client, requested string) (api.UserMachine, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return api.UserMachine{}, err
	}
	for _, machine := range machines {
		if machine.ID == requested {
			return machine, nil
		}
	}
	var matches []api.UserMachine
	for _, machine := range machines {
		if strings.EqualFold(machine.Alias, requested) {
			matches = append(matches, machine)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, machine := range matches {
			ids = append(ids, machine.ID)
		}
		return api.UserMachine{}, fmt.Errorf("%w: %q matches machine IDs %s; use an exact ID", resolver.ErrMachineAmbiguous, requested, strings.Join(ids, ", "))
	}
	return resolveUserMachine(ctx, client, requested)
}

func newSSHOperationID() string {
	return "operation_" + uuid.NewString()
}

func sshConnectInfo(machine api.UserMachine, descriptor api.SSHDescriptor) resolver.ConnectInfo {
	return resolver.ConnectInfo{
		TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineState: machine.State,
		MachineGeneration: uint64(machine.InstallationGeneration), TunnelTarget: descriptor.Endpoints.WSS,
		Terminal: &resolver.TerminalTarget{Protocol: "paperboat.ssh.v1", EnvironmentID: descriptor.Environment.ID, QUICEndpoint: descriptor.Endpoints.QUIC, WSSEndpoint: descriptor.Endpoints.WSS, Auth: resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.Format(time.RFC3339Nano), Scopes: descriptor.Auth.Scopes, ResourceID: descriptor.Auth.AccessSessionID}, CWD: descriptor.Environment.Root},
	}
}

func pendingSSHDescriptor(machine api.UserMachine, operationID string) api.SSHDescriptor {
	return api.SSHDescriptor{OperationID: operationID, ExpiresAt: time.Now().Add(2 * time.Minute), Auth: api.AuthMaterial{ExpiresAt: time.Now().Add(2 * time.Minute)}, Environment: &api.Environment{ID: machine.EnvironmentID, Kind: "machine", ResourceID: machine.ID, Root: machine.WorkspaceRoot}}
}

func parseExecEnvironment(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	namePattern := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for _, value := range values {
		name, item, ok := strings.Cut(value, "=")
		if !ok || !namePattern.MatchString(name) || strings.ContainsRune(item, '\x00') {
			return nil, localArgumentError("Each --env must use a valid variable name followed by =value; values cannot contain NUL bytes.")
		}
		if _, exists := result[name]; exists {
			return nil, localArgumentError("An --env variable was specified more than once; provide each variable once.")
		}
		result[name] = item
	}
	return result, nil
}

func actionRemoteExec(c *command.Context, requested string, request tunnel.ExecRequest, jsonOutput bool) (returnErr error) {
	if len(request.Argv) == 0 || request.Timeout < 0 || request.Timeout > 24*time.Hour {
		return invocationError(errors.New("exec timeout must be between zero and 24h and argv must not be empty"))
	}
	request.OperationID = newExecOperationID()
	fail := func(code int, errorCode string, changed, uncertain bool, err error) error {
		if !jsonOutput {
			return err
		}
		if encodeErr := writeExecJSONFailure(c.Writer, request.OperationID, errorCode, safeExecError(err), changed, uncertain); encodeErr != nil {
			return encodeErr
		}
		return execCommandFailure{cause: err, code: code, presented: true}
	}
	d, err := buildDeps(c)
	if err != nil {
		return fail(255, "local_configuration", false, false, err)
	}
	if d.peerApplications == nil || d.cfg.ServerURL == "" {
		err = errors.New("Paperboat server and peer transport are required for remote execution")
		return fail(255, "local_configuration", false, false, err)
	}
	credential, err := d.auth.Credential()
	if onlyEnvironmentFailureLeaves(err, func(leaf error) bool { return leaf == config.ErrNoCredentials }) {
		err = fmt.Errorf("authentication required: %w", err)
		return fail(255, "authentication_required", false, false, err)
	}
	if err != nil {
		return fail(255, "authentication_unavailable", false, false, err)
	}
	client, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, credential)
	if err != nil {
		return fail(255, "workspace_unavailable", false, false, err)
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return fail(255, "source_identity_unavailable", false, false, err)
	}
	if err := requireLocalDaemonService(c.Context, d.cfg); err != nil {
		return fail(255, "local_transport_unavailable", false, false, fmt.Errorf("prepare local peer transport: %w", err))
	}
	client.SetSourceMachineID(sourceMachineID)
	machine, warm := resolveWarmUserMachine(c.Context, requested)
	if !warm {
		machine, err = resolveUserMachine(c.Context, client, requested)
	}
	if err != nil {
		err = friendlyCommandError(err)
		return fail(255, "machine_resolution_failed", false, false, err)
	}
	if machine.InstallationGeneration < 1 {
		err = errors.New("machine has no active runtime generation")
		return fail(255, "runtime_unavailable", false, false, err)
	}
	// The daemon owns operation-descriptor issuance. This placeholder carries
	// only inventory authority and is replaced with fresh endpoints/credentials
	// inside the authenticated local API broker.
	descriptor := api.ExecDescriptor{OperationID: request.OperationID, ExpiresAt: time.Now().Add(2 * time.Minute), Auth: api.AuthMaterial{ExpiresAt: time.Now().Add(2 * time.Minute)}, Environment: &api.Environment{ID: machine.EnvironmentID, Kind: "machine", ResourceID: machine.ID, Root: machine.WorkspaceRoot}}
	if request.CWD == "" {
		request.CWD = machine.WorkspaceRoot
	}
	if !remoteAbsolutePath(machine.Platform, machine.WorkspaceRoot, request.CWD) {
		err = invocationError(errors.New("--cwd must be an absolute path"))
		return fail(2, "invalid_request", false, false, err)
	}
	if request.PTY {
		request.Columns, request.Rows = localTerminalSize()
		if request.Columns == 0 || request.Rows == 0 {
			request.Columns, request.Rows = 80, 24
		}
	}
	dial := func(current api.ExecDescriptor) (tunnel.ExecConn, error) {
		// Keep caller cancellation attached while dialing, then detach the
		// established carrier so actionRemoteExec can complete the remote cancel
		// RPC before transport teardown.
		dialCtx, cancelDial := context.WithCancel(context.WithoutCancel(c.Context))
		defer cancelDial()
		stopCallerCancel := context.AfterFunc(c.Context, cancelDial)
		connection, dialErr := d.peerApplications.DialExec(dialCtx, execConnectInfo(machine, current), request)
		if dialErr != nil {
			stopCallerCancel()
			cancelDial()
			return nil, dialErr
		}
		if !stopCallerCancel() || c.Context.Err() != nil {
			cancelDial()
			_ = connection.Close()
			return nil, c.Context.Err()
		}
		return connection, nil
	}
	connection, err := dial(descriptor)
	initialUncertain := false
	for attempt := 1; err != nil && attempt < 3; attempt++ {
		var uncertain *tunnel.ExecStartUncertainError
		if !errors.As(err, &uncertain) && !errors.Is(err, localapi.ErrExecStartUncertain) {
			break
		}
		initialUncertain = true
		observeExecFailure(c.Context, err)
		if waitErr := waitExecRetry(c.Context, attempt); waitErr != nil {
			err = errors.Join(err, waitErr)
			break
		}
		connection, err = dial(descriptor)
	}
	if err != nil {
		var uncertain *tunnel.ExecStartUncertainError
		if initialUncertain || errors.As(err, &uncertain) || errors.Is(err, localapi.ErrExecStartUncertain) {
			return fail(255, "exec_start_uncertain", true, true, err)
		}
		return fail(255, "transport_unavailable", false, false, err)
	}
	connectionRef := newExecConnectionRef(connection)
	workerCtx, stopWorkers := context.WithCancel(context.WithoutCancel(c.Context))
	inputCtx, stopInput := context.WithCancel(workerCtx)
	execDone := make(chan struct{})
	var workers sync.WaitGroup
	var restoreTerminal func() error
	defer func() {
		close(execDone)
		stopWorkers()
		if active := connectionRef.Current(); active != nil {
			if abortErr := abortExecAttachment(active); abortErr != nil {
				observeExecFailure(workerCtx, abortErr)
			}
		}
		workers.Wait()
		if restoreTerminal != nil {
			returnErr = appendExecFailure(returnErr, restoreTerminal())
		}
	}()
	processSignals := make(chan os.Signal, 1)
	var cancelRequested atomic.Bool
	cancelResult := make(chan execCancelOutcome, 1)
	cancelRemote := func() execCancelOutcome {
		// A separate idempotent attachment avoids queuing cancellation behind output.
		ctx, cancel := context.WithTimeout(workerCtx, 25*time.Second)
		defer cancel()
		var failures []error
		for attempt := 0; attempt < 2; attempt++ {
			retryDescriptor, descriptorErr := client.MachineExecDescriptor(ctx, machine.ID, request.OperationID)
			if descriptorErr != nil {
				return execCancelOutcome{err: errors.Join(append(failures, descriptorErr)...)}
			}
			replacement, dialErr := d.peerApplications.DialExec(ctx, execConnectInfo(machine, retryDescriptor), request)
			if dialErr != nil {
				failures = append(failures, dialErr)
				observeExecFailure(ctx, dialErr)
				continue
			}
			outcome := cancelExecAttachment(ctx, replacement)
			if outcome.terminal != nil {
				return outcome
			}
			failures = append(failures, outcome.err)
			observeExecFailure(ctx, outcome.err)
		}
		return execCancelOutcome{err: errors.Join(failures...)}
	}
	startCancel := func() {
		if !cancelRequested.CompareAndSwap(false, true) {
			return
		}
		stopInput()
		workers.Add(1)
		go func() { defer workers.Done(); cancelResult <- cancelRemote() }()
	}
	// SIGINT cancels the remote process; hangup remains an explicit process signal.
	signal.Notify(processSignals, syscall.SIGHUP, os.Interrupt)
	defer signal.Stop(processSignals)
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case received := <-processSignals:
				if received == os.Interrupt {
					startCancel()
				} else if active := connectionRef.Current(); active != nil {
					if signalErr := active.Signal(received.String()); signalErr != nil {
						observeExecFailure(workerCtx, signalErr)
					}
				}
			case <-c.Context.Done():
				startCancel()
				return
			case <-execDone:
				return
			}
		}
	}()
	if request.PTY && term.IsTerminal(int(os.Stdin.Fd())) {
		state, rawErr := term.MakeRaw(int(os.Stdin.Fd()))
		if rawErr != nil {
			return rawErr
		}
		restoreTerminal = func() error { return term.Restore(int(os.Stdin.Fd()), state) }
		resizeSignals := make(chan os.Signal, 1)
		notifyResizeSignals(resizeSignals)
		defer signal.Stop(resizeSignals)
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-resizeSignals:
					cols, rows := localTerminalSize()
					if cols > 0 && rows > 0 {
						if active := connectionRef.Current(); active != nil {
							if resizeErr := active.Resize(rows, cols); resizeErr != nil {
								observeExecFailure(workerCtx, resizeErr)
							}
						}
					}
				case <-execDone:
					return
				}
			}
		}()
	}
	inputResult := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); inputResult <- forwardExecInput(inputCtx, os.Stdin, connectionRef) }()
	encoder := json.NewEncoder(c.Writer)
	sawTerminalEvent := false
	emittedStarted := false
	lastEventSequence := uint64(0)
	reattachments := 0
	var code int
	var inputCloseFailure error
	contextDone := c.Context.Done()
	var cancelDeadline <-chan time.Time
	var cancelTimer *time.Timer
	defer func() {
		if cancelTimer != nil {
			cancelTimer.Stop()
		}
	}()
	for {
		events := connection.Events()
		streamEnded := false
		for !streamEnded {
			select {
			case event, ok := <-events:
				if !ok {
					streamEnded = true
					continue
				}
				if event.EventSequence > lastEventSequence {
					lastEventSequence = event.EventSequence
				}
				if event.State == "started" {
					if emittedStarted {
						continue
					}
					emittedStarted = true
				}
				terminalEvent := event.Stream == "" && event.State != "" && event.State != "started"
				if terminalEvent {
					sawTerminalEvent = true
				}
				if writeErr := writeExecEvent(c, encoder, event, request.PTY, jsonOutput); writeErr != nil {
					return execCommandFailure{cause: writeErr, code: 255, presented: jsonOutput}
				}
				if terminalEvent {
					streamEnded = true
				}
			case inputErr := <-inputResult:
				inputResult = nil
				if inputErr != nil {
					if cancelRequested.Load() && classifyCommandFailure(inputErr).kind == commandCanceled {
						continue
					}
					if execInputClosedOnly(inputErr) {
						inputCloseFailure = inputErr
					} else {
						err = appendExecFailure(err, inputErr)
						streamEnded = true
					}
				}
			case outcome := <-cancelResult:
				err = appendExecFailure(err, outcome.err)
				code = outcome.code
				if outcome.terminal != nil && !sawTerminalEvent {
					if writeErr := writeExecEvent(c, encoder, *outcome.terminal, request.PTY, jsonOutput); writeErr != nil {
						return execCommandFailure{cause: appendExecFailure(err, writeErr), code: 255, presented: jsonOutput}
					}
					sawTerminalEvent = true
				}
				if c.Context.Err() == context.DeadlineExceeded && (outcome.terminal == nil || outcome.terminal.State == "canceled") {
					err = appendExecFailure(err, context.DeadlineExceeded)
				}
				if abortErr := abortExecAttachment(connection); abortErr != nil {
					observeExecFailure(workerCtx, abortErr)
				}
				streamEnded = true
			case <-contextDone:
				contextDone = nil
				startCancel()
				if cancelDeadline == nil {
					cancelTimer = time.NewTimer(30 * time.Second)
					cancelDeadline = cancelTimer.C
				}
			case <-cancelDeadline:
				err = errors.New("remote execution cancellation outcome is uncertain")
				_ = abortExecAttachment(connection)
				streamEnded = true
			}
		}
		if err == nil {
			code, err = connection.Wait()
		}
		if cancelRequested.Load() || c.Context.Err() != nil {
			break
		}
		if err == nil || !errors.Is(err, tunnel.ErrTransportLost) || classifyCommandFailure(err).kind == commandUnexpected || sawTerminalEvent || reattachments >= 2 || c.Context.Err() != nil {
			break
		}
		observeExecFailure(c.Context, appendExecFailure(err, inputCloseFailure))
		connectionRef.Clear(connection)
		if abortErr := abortExecAttachment(connection); abortErr != nil {
			observeExecFailure(c.Context, abortErr)
		}
		request.FromSequence = lastEventSequence + 1
		if request.FromSequence == 0 {
			request.FromSequence = 1
		}
		previousFailure := err
		var replacement tunnel.ExecConn
		for reattachments < 2 {
			reattachments++
			if waitErr := waitExecRetry(c.Context, reattachments); waitErr != nil {
				err = errors.Join(err, waitErr)
				break
			}
			descriptor, err = client.MachineExecDescriptor(c.Context, machine.ID, request.OperationID)
			if err != nil {
				break
			}
			replacement, err = dial(descriptor)
			if err == nil {
				break
			}
			var uncertain *tunnel.ExecStartUncertainError
			if !errors.As(err, &uncertain) && !errors.Is(err, localapi.ErrExecStartUncertain) {
				break
			}
		}
		if err != nil || replacement == nil {
			err = appendExecFailure(previousFailure, err)
			break
		}
		inputCloseFailure = nil
		connection = replacement
		if setErr := connectionRef.Set(connection); setErr != nil {
			err = setErr
			break
		}
	}
	if err != nil {
		err = appendExecFailure(err, inputCloseFailure)
	}
	return finishExecResult(c.Writer, c.ErrWriter, request.OperationID, jsonOutput, sawTerminalEvent, code, err)
}

// A cancellation attachment owns only its transport and output drain. The
// deadline interrupts actual network writes/ack reads and joins both workers.
type execCancelOutcome struct {
	code     int
	err      error
	terminal *tunnel.ExecEvent
}

func cancelExecAttachment(ctx context.Context, connection tunnel.ExecConn) execCancelOutcome {
	drainDone := make(chan struct{})
	var terminal *tunnel.ExecEvent
	go func() {
		defer close(drainDone)
		for event := range connection.Events() {
			if event.Stream == "" && event.State != "" && event.State != "started" {
				copy := event
				terminal = &copy
			}
		}
	}()
	hookDone := make(chan struct{})
	stopHook := context.AfterFunc(ctx, func() { defer close(hookDone); _ = abortExecAttachment(connection) })
	cancelErr := connection.Cancel()
	code := 0
	if cancelErr == nil {
		code, cancelErr = connection.Wait()
	}
	if !stopHook() {
		<-hookDone
	}
	abortErr := abortExecAttachment(connection)
	<-drainDone
	if terminal != nil {
		if abortErr != nil {
			observeExecFailure(ctx, abortErr)
		}
		return execCancelOutcome{code: code, err: cancelErr, terminal: terminal}
	}
	return execCancelOutcome{code: code, err: appendExecFailure(appendExecFailure(cancelErr, abortErr), ctx.Err())}
}

func appendExecFailure(previous, next error) error {
	if previous == nil {
		return next
	}
	if next == nil {
		return previous
	}
	return errors.Join(previous, next)
}

func abortExecAttachment(connection tunnel.ExecConn) error {
	if aborter, ok := connection.(interface{ Abort() error }); ok {
		return aborter.Abort()
	}
	return connection.Detach()
}
func observeExecFailure(ctx context.Context, err error) {
	if err == nil || errorreport.HTTPAttemptObserved(err) {
		return
	}
	failure := classifyCommandFailure(err)
	if failure.kind == commandCanceled || failure.kind == commandInteractiveCanceled {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "pb", "exec", "command", "command_failed", err)
}

func remoteAbsolutePath(platform, workspaceRoot, path string) bool {
	return remotepath.AbsoluteForTarget(platform, workspaceRoot, path)
}

func finishExecResult(stdout, stderr io.Writer, operationID string, jsonOutput, sawTerminalEvent bool, code int, err error) error {
	if err == nil && !sawTerminalEvent {
		err = tunnel.ErrTransportLost
	}
	if err == nil {
		if code != 0 {
			return exitCodeError{code: code}
		}
		return nil
	}
	reserved := 255
	if execCancellationOnly(err) {
		if classifyCommandFailure(err).kind == commandDeadline {
			reserved = 124
		} else {
			reserved = 130
		}
	}
	if remote, ok := soleExecRemoteError(err); ok {
		switch remote.Code {
		case "exec_timeout":
			reserved = 124
		case "exec_canceled", "canceled":
			reserved = 130
		default:
			if publicExecErrorCode(remote.Code) == remote.Code {
				reserved = 125
			}
		}
	}
	if jsonOutput {
		if !sawTerminalEvent {
			if encodeErr := writeExecJSONFailure(stdout, operationID, "transport_lost", safeExecError(err), true, true); encodeErr != nil {
				return errors.Join(err, encodeErr)
			}
		}
		return execCommandFailure{cause: err, code: reserved, presented: true}
	}
	if _, writeErr := fmt.Fprintln(stderr, "pb:", safeExecError(err)); writeErr != nil {
		return errors.Join(err, writeErr)
	}
	return execCommandFailure{cause: err, code: reserved, presented: true}
}

// Only a fully traversed cancellation/deadline tree can claim a certain
// cancellation result. Bounds, cycles and unrelated failures fail closed.
func execInputClosedOnly(err error) bool {
	pending := []error{err}
	seen := make(map[error]bool)
	for count := 0; len(pending) > 0 && count < 32; count++ {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if current == io.EOF || current == io.ErrClosedPipe || current == net.ErrClosed || current == syscall.EPIPE {
			continue
		}
		if !value.Type().Comparable() || seen[current] {
			return false
		}
		seen[current] = true
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > 32-len(pending) {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			pending = append(pending, wrapped.Unwrap())
			continue
		}
		return false
	}
	return len(pending) == 0
}

func execCancellationOnly(err error) bool {
	pending := []error{err}
	seen := make(map[error]bool)
	for count := 0; len(pending) > 0 && count < 32; count++ {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if current == context.Canceled || current == context.DeadlineExceeded {
			continue
		}
		if remote, ok := current.(*tunnel.RemoteExecError); ok {
			if remote.Code == "exec_canceled" || remote.Code == "canceled" || remote.Code == "exec_timeout" {
				continue
			}
			return false
		}
		if !value.Type().Comparable() || seen[current] {
			return false
		}
		seen[current] = true
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > 32-len(pending) {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			pending = append(pending, wrapped.Unwrap())
			continue
		}
		return false
	}
	return len(pending) == 0
}

func soleExecRemoteError(err error) (*tunnel.RemoteExecError, bool) {
	for i := 0; err != nil && i < 32; i++ {
		if remote, ok := err.(*tunnel.RemoteExecError); ok {
			return remote, remote != nil
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return nil, false
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = wrapper.Unwrap()
	}
	return nil, false
}

func writeExecJSONFailure(writer io.Writer, operationID, errorCode, detail string, changed, uncertain bool) error {
	return json.NewEncoder(writer).Encode(struct {
		Version     string `json:"version"`
		Event       string `json:"event"`
		OperationID string `json:"operation_id"`
		ErrorCode   string `json:"error_code"`
		Detail      string `json:"detail,omitempty"`
		Changed     bool   `json:"changed"`
		Uncertain   bool   `json:"uncertain,omitempty"`
	}{Version: "paperboat.exec-event/v1", Event: "failed", OperationID: operationID, ErrorCode: errorCode, Detail: detail, Changed: changed, Uncertain: uncertain})
}

func publicExecErrorCode(code string) string {
	switch code {
	case "", "exec_timeout", "exec_canceled", "exec_result_unavailable", "exec_failed", "exec_start_failed", "exec_start_uncertain", "exec_cancel_failed", "exec_wait_failed", "exec_already_running", "failed", "canceled":
		return code
	default:
		return "exec_failed"
	}
}

func safeExecError(err error) string {
	if err == nil {
		return ""
	}
	if remote, ok := soleExecRemoteError(err); ok {
		switch remote.Code {
		case "exec_timeout":
			return "Remote execution exceeded its time limit. Check the operation before retrying."
		case "exec_canceled", "canceled":
			return "Remote execution was canceled."
		case "exec_result_unavailable":
			return "The remote execution result is unavailable. Check the machine before retrying."
		}
	}
	return "Remote execution did not finish. Check the machine connection and retry; the remote outcome may be unknown."
}

func execConnectInfo(machine api.UserMachine, descriptor api.ExecDescriptor) resolver.ConnectInfo {
	return resolver.ConnectInfo{
		TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineState: machine.State,
		MachineGeneration: uint64(machine.InstallationGeneration), TunnelTarget: descriptor.Endpoints.WSS,
		Terminal: &resolver.TerminalTarget{Protocol: "paperboat.exec.v1", EnvironmentID: descriptor.Environment.ID, QUICEndpoint: descriptor.Endpoints.QUIC, WSSEndpoint: descriptor.Endpoints.WSS, Auth: resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.Format(time.RFC3339Nano), Scopes: descriptor.Auth.Scopes, ResourceID: descriptor.Auth.AccessSessionID}, CWD: descriptor.Environment.Root},
	}
}

func waitExecRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(attempt) * 100 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func writeExecEvent(c *command.Context, encoder *json.Encoder, event tunnel.ExecEvent, pty, jsonOutput bool) error {
	if jsonOutput {
		wire := struct {
			Version     string             `json:"version"`
			Event       string             `json:"event"`
			OperationID string             `json:"operation_id"`
			Stream      string             `json:"stream,omitempty"`
			Sequence    *uint64            `json:"sequence,omitempty"`
			Data        []byte             `json:"data,omitempty"`
			State       string             `json:"state,omitempty"`
			Result      *tunnel.ExecResult `json:"result,omitempty"`
			ErrorCode   string             `json:"error_code,omitempty"`
			Changed     *bool              `json:"changed,omitempty"`
			Uncertain   bool               `json:"uncertain,omitempty"`
		}{Version: "paperboat.exec-event/v1", Event: execEventName(event), OperationID: event.OperationID, Stream: event.Stream, Sequence: execEventSequence(event), Data: event.Data, State: event.State, Result: event.Result, ErrorCode: publicExecErrorCode(event.ErrorCode), Changed: execEventChanged(event)}
		return encoder.Encode(wire)
	}
	if len(event.Data) == 0 {
		return nil
	}
	writer := c.Writer
	if event.Stream == "stderr" && !pty {
		writer = c.ErrWriter
	}
	_, err := writer.Write(event.Data)
	return err
}

type execConnectionRef struct {
	mu      sync.Mutex
	conn    tunnel.ExecConn
	changed chan struct{}
	eof     bool
}

func newExecConnectionRef(connection tunnel.ExecConn) *execConnectionRef {
	return &execConnectionRef{conn: connection, changed: make(chan struct{})}
}

func (r *execConnectionRef) Current() tunnel.ExecConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn
}

func (r *execConnectionRef) Set(connection tunnel.ExecConn) error {
	r.mu.Lock()
	r.conn = connection
	eof := r.eof
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	if eof {
		return connection.CloseWrite()
	}
	return nil
}

func (r *execConnectionRef) Clear(connection tunnel.ExecConn) {
	r.mu.Lock()
	if r.conn == connection {
		r.conn = nil
		close(r.changed)
		r.changed = make(chan struct{})
	}
	r.mu.Unlock()
}

func (r *execConnectionRef) Wait(ctx context.Context) (tunnel.ExecConn, error) {
	for {
		r.mu.Lock()
		connection, changed := r.conn, r.changed
		r.mu.Unlock()
		if connection != nil {
			return connection, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *execConnectionRef) CloseInput() error {
	r.mu.Lock()
	r.eof = true
	connection := r.conn
	r.mu.Unlock()
	if connection != nil {
		return connection.CloseWrite()
	}
	return nil
}

func forwardExecInput(ctx context.Context, reader io.Reader, connections *execConnectionRef) error {
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := readExecInput(ctx, reader, buffer)
		if n < 0 || n > len(buffer) {
			return io.ErrShortBuffer
		}
		if n > 0 {
			connection, err := connections.Wait(ctx)
			if err != nil {
				return err
			}
			remaining := buffer[:n]
			for len(remaining) > 0 {
				written, writeErr := connection.Write(remaining)
				if writeErr != nil || written <= 0 || written > len(remaining) {
					if writeErr == nil {
						writeErr = io.ErrShortWrite
					}
					connections.Clear(connection)
					abortErr := abortExecAttachment(connection)
					cause := appendExecFailure(writeErr, abortErr)
					if execInputClosedOnly(cause) || classifyCommandFailure(cause).kind == commandUnexpected {
						return cause
					}
					observeExecFailure(ctx, cause)
					break
				}
				remaining = remaining[written:]
			}
		}
		if n == 0 && readErr == nil {
			return io.ErrNoProgress
		}
		if readErr != nil {
			if readErr == io.EOF {
				return connections.CloseInput()
			}
			return readErr
		}
	}
}

func execEventName(event tunnel.ExecEvent) string {
	if event.Stream != "" {
		return event.Stream
	}
	if event.State == "started" {
		return "started"
	}
	if event.State == "exited" && event.Result != nil && event.Result.Signal != "" {
		return "signaled"
	}
	if event.State == "exited" || event.State == "signaled" {
		return event.State
	}
	return "failed"
}

func execEventChanged(event tunnel.ExecEvent) *bool {
	if event.Stream != "" || event.State == "started" {
		return nil
	}
	changed := event.State == "exited" || event.State == "signaled" || event.State == "canceled" || event.Result != nil
	return &changed
}

func execEventSequence(event tunnel.ExecEvent) *uint64 {
	if event.Stream == "" {
		return nil
	}
	sequence := event.Sequence
	return &sequence
}

func newExecOperationID() string {
	return "operation_" + uuid.NewString()
}

func localTerminalTarget() (string, string, error) {
	machineID, err := configuredMachineID()
	if err != nil {
		return "", "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("read current directory: %w", err)
	}
	return machineID, cwd, nil
}

func actionConnectTarget(c *command.Context, requested string) error {
	return actionConnectTargetInDirectory(c, requested, "")
}

func actionConnectTargetInDirectory(c *command.Context, requested, cwd string) error {
	machineRef := strings.TrimSpace(requested)
	if machineRef == "" {
		machineRef = c.Args().First()
	}
	if machineRef == "" && strings.TrimSpace(c.String("server")) != "" {
		cfg, err := config.Load(c.String("config"))
		if err != nil {
			return err
		}
		cfg.ServerURL, err = config.NormalizeServerURL(c.String("server"))
		if err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Fprintln(c.Writer, cfg.Path())
		return nil
	}
	if c.Args().Len() > 2 {
		return localArgumentError("expected an environment and optional `new`")
	}
	if c.Args().Len() == 2 && c.Args().Get(1) != "new" {
		return localArgumentError("second argument must be `new`")
	}
	if c.Args().Len() == 2 && strings.TrimSpace(c.String("session")) != "" {
		return localArgumentError("`new` and --session cannot be used together")
	}
	if strings.TrimSpace(c.String("name")) != "" && strings.TrimSpace(c.String("session")) != "" {
		return localArgumentError("--name and --session cannot be used together")
	}

	ctx, cancelConnection := context.WithCancel(c.Context)
	defer cancelConnection()
	ownedCommand := *c
	ownedCommand.Context = ctx
	c = &ownedCommand
	d, err := buildDeps(c)
	if err != nil {
		return err
	}
	if strings.TrimSpace(d.cfg.ServerURL) == "" {
		return commandRejection{reason: commandRejectMissingServer}
	}
	cred, err := d.auth.Credential()
	if err != nil {
		return err
	}
	backend, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, cred)
	if err != nil {
		return err
	}
	if machineRef == "" {
		machineRef, err = defaultWorkspaceEnvironment(c.Context, c, backend, d.cfg)
		if err != nil {
			return err
		}
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return err
	}
	backend.SetSourceMachineID(sourceMachineID)
	statusConfig := d.cfg.StatusBar
	if value := strings.TrimSpace(c.String("status-bar")); value != "" {
		statusConfig.Mode = strings.ToLower(value)
	}
	if value := strings.TrimSpace(c.String("status-bar-fullscreen")); value != "" {
		statusConfig.Fullscreen = strings.ToLower(value)
	}
	if value := strings.TrimSpace(c.String("status-bar-theme")); value != "" {
		statusConfig.Theme = strings.ToLower(value)
	}
	bar := statusbar.New(statusbar.Options{
		Mode:          statusConfig.Mode,
		Fullscreen:    statusConfig.Fullscreen,
		Theme:         statusConfig.Theme,
		Privacy:       statusConfig.Privacy,
		TerminalTitle: statusConfig.TerminalTitle,
		Colors: statusbar.Colors{
			Foreground: statusConfig.Colors.Foreground,
			Background: statusConfig.Colors.Background,
			Accent:     statusConfig.Colors.Accent,
			Warning:    statusConfig.Colors.Warning,
			Error:      statusConfig.Colors.Error,
		},
		NoticeDuration: time.Duration(d.cfg.StatusBar.NoticeSeconds) * time.Second,
		Layout: statusbar.Layout{
			Left:   d.cfg.StatusBar.Left,
			Center: d.cfg.StatusBar.Center,
			Right:  d.cfg.StatusBar.Right,
		},
	})
	if c.Bool("debug") {
		bar.SetDebugVersions(buildinfo.Version, "")
	}
	defer func() { _ = bar.Close() }()
	useStatusBar := bar.Enabled()
	observeTransferFailure := func(cause error) {
		reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", cause)
		if reference == "" {
			return
		}
		if useStatusBar {
			bar.FailureFor("file_transfer", "File transfer unavailable · "+reference)
		} else {
			fmt.Fprintf(os.Stderr, "File transfer is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: %s.\n", reference)
		}
	}
	var closeTelemetry func()
	d.telemetry, closeTelemetry = connectTelemetry(d.cfg, os.Stderr)
	defer closeTelemetry()

	sessionName := c.String("name")
	sessionRef := c.String("session")
	target, resolvedMachine, err := resolveEnvironmentTargetWithMachine(c.Context, backend, machineRef)
	if err != nil {
		return err
	}
	var createSession *resolver.TerminalSessionCreate
	terminalSessionID := ""
	terminalSessionName := ""
	if strings.TrimSpace(sessionRef) != "" {
		session, err := resolveTerminalSession(c.Context, backend, target, sessionRef)
		if err != nil {
			return friendlyCommandError(err)
		}
		terminalSessionID = session.ID
		terminalSessionName = session.Name
	} else if err := validateSessionNameOptional(sessionName); err != nil {
		return err
	} else {
		// Create the durable session and fetch its connection descriptor in
		// one round trip; the idempotency key binds descriptor retries.
		createSession = &resolver.TerminalSessionCreate{Name: sessionName, IdempotencyKey: newIdempotencyKey(), CWD: cwd}
		terminalSessionName = sessionName
	}
	bar.SetIdentity(machineRef, terminalSessionName)
	newResolver := func(credential config.Credential) (*resolver.APIResolver, error) {
		client, err := newWorkspaceAPIClient(c, d.cfg.ServerURL, credential)
		if err != nil {
			return nil, err
		}
		client.SetSourceMachineID(sourceMachineID)
		apiResolver := resolver.NewAPIResolver(client, d.cfg)
		apiResolver.Telemetry = d.telemetry
		return apiResolver, nil
	}
	d.resolver, err = newResolver(cred)
	if err != nil {
		return err
	}
	if apiResolver, ok := d.resolver.(*resolver.APIResolver); ok {
		apiResolver.Progress = func(status, reason string, retryAfter time.Duration) {
			if useStatusBar {
				bar.SetConnection("connecting")
				bar.Loading("Preparing connection")
				return
			}
			fmt.Fprintf(os.Stderr, "Connecting: %s (%s), retrying in %s...\n", status, reason, retryAfter.Round(time.Second))
		}
	}
	remoteSize := func() (uint16, uint16) {
		if useStatusBar {
			if cols, rows := bar.RemoteSize(); cols > 0 && rows > 0 {
				return cols, rows
			}
		}
		return localTerminalSize()
	}

	var info resolver.ConnectInfo
	var conn tunnel.Conn
	if d.peerTunnel != nil {
		defer d.peerTunnel.Close()
	}
	var lastTerminalSequence atomic.Int64
	recordTerminalSequence := func(sequence int) {
		for {
			current := lastTerminalSequence.Load()
			if int64(sequence) <= current || lastTerminalSequence.CompareAndSwap(current, int64(sequence)) {
				return
			}
		}
	}
	recordReplayGap := func(requested, earliest, _ uint64) {
		missing := uint64(0)
		if earliest > requested {
			missing = earliest - requested
		}
		event := telemetry.Event{Name: "terminal.replay_gap", At: time.Now(), Outcome: "recovered", MachineID: info.MachineID, Count: int64(min(missing, math.MaxInt64))}
		if info.Terminal != nil {
			event.EnvironmentID = info.Terminal.EnvironmentID
		}
		if event.Validate() == nil {
			d.telemetry.Record(event)
		}
	}
	var transferClient *filetransfer.NativeClient
	var transferLease *localapi.FileTransferLease
	for attempt := 0; attempt <= d.cfg.Connect.DialRetries; attempt++ {
		resolveRequest := resolver.ConnectRequest{Machine: machineRef, Credential: cred, TerminalSessionID: terminalSessionID, CreateTerminalSession: createSession}
		if target.kind == environmentUserMachine && resolvedMachine.ID != "" && resolvedMachine.InstallationGeneration > 0 {
			resolveRequest.ResolvedMachine = &resolver.ResolvedMachine{ID: resolvedMachine.ID, Name: resolvedMachine.Alias, State: resolvedMachine.State, Generation: uint64(resolvedMachine.InstallationGeneration)}
		}
		info, err = d.resolver.Resolve(ctx, resolveRequest)
		if err == nil {
			if createSession != nil && info.TerminalSession != nil {
				created := info.TerminalSession
				if terminalSessionID == "" {
					terminalSessionID = created.ID
				}
				if created.Name != "" && created.Name != terminalSessionName {
					terminalSessionName = created.Name
					bar.SetIdentity(machineRef, terminalSessionName)
				}
				if created.EvictedSession != nil {
					fmt.Fprintf(os.Stderr, "Session limit reached; removed least-recent session %q (%s).\n", created.EvictedSession.Name, created.EvictedSession.State)
				}
			}
			if info.Terminal != nil {
				info.Terminal.Debug = c.Bool("debug")
				info.Terminal.RestartIfNotRunning = true
				info.Terminal.ReplayHistory = true
				info.Terminal.SequenceSink = recordTerminalSequence
				info.Terminal.ReplayGapSink = recordReplayGap
				info.Terminal.Env = forwardedTerminalEnv(config.TerminalEnv)
				info.Terminal.Cols, info.Terminal.Rows = remoteSize()
			}
			if transferLease != nil {
				observeTransferFailure(transferLease.Close())
				transferLease = nil
			}
			transferClient = nil
			if info.FileTransfer != nil && d.peerLocal != nil {
				var transferErr error
				transferClient, transferLease, transferErr = nativeFileTransferClient(ctx, d.peerLocal, info, newIdempotencyKey())
				observeTransferFailure(transferErr)
			}
			// The file-transfer policy check runs concurrently with the
			// transport dial. Paste availability is decided before any input
			// is accepted, but the health check round trip never delays the
			// shell becoming interactive.
			verifyDone := make(chan error, 1)
			var clientToVerify *filetransfer.Client
			if transferClient != nil {
				clientToVerify = transferClient.Client
			}
			if clientToVerify != nil {
				policyToVerify := descriptorFileTransferPolicy(info.FileTransfer)
				go func() { verifyDone <- clientToVerify.VerifyPolicy(ctx, policyToVerify) }()
			} else {
				verifyDone <- nil
			}
			conn, err = d.tunnel.Dial(ctx, info)
			if policyErr := <-verifyDone; policyErr != nil {
				observeTransferFailure(policyErr)
				transferClient = nil
				if transferLease != nil {
					observeTransferFailure(transferLease.Close())
					transferLease = nil
				}
				if useStatusBar {
					bar.FailureFor("file_transfer", "File transfer unavailable")
				} else {
					fmt.Fprintln(os.Stderr, "File transfer is unavailable for this connection. Terminal access will continue.")
				}
			}
		}
		if err == nil {
			break
		}
		if errors.Is(err, api.ErrUnauthenticated) {
			return err
		}
		if attempt == d.cfg.Connect.DialRetries || !retryableInitialConnectError(err) {
			break
		}
		if useStatusBar {
			bar.SetConnection("reconnecting")
			bar.Loading("Retrying connection")
		} else {
			fmt.Fprintf(os.Stderr, "Connection attempt %d failed; refreshing the descriptor in %ds...\n", attempt+1, d.cfg.Connect.DialRetrySeconds)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(time.Duration(d.cfg.Connect.DialRetrySeconds) * time.Second):
		}
	}
	if err != nil {
		if useStatusBar {
			bar.SetConnection("failed")
			bar.FailureFor("connection", "Connection failed")
		}

		return fmt.Errorf("connect to %q: %w", machineRef, err)
	}
	if c.Bool("debug") {
		bar.SetDebugVersions(buildinfo.Version, tunnel.TerminalRuntimeVersion(conn))
	}
	defer func() {
		if transferLease != nil {
			observeTransferFailure(transferLease.Close())
		}
	}()
	if err := rememberWorkspaceEnvironment(c, d.cfg, info.MachineID); err != nil {
		closeErr := conn.Close()
		return fmt.Errorf("remember connected environment: %w", errors.Join(err, closeErr))
	}
	if useStatusBar {
		bar.SetConnection("connected")
		bar.Notice("Connected")
		bar.ClearRemoteViewport()
	} else if term.IsTerminal(int(os.Stdout.Fd())) {
		_, _ = fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	}
	if useStatusBar && d.peerLocal != nil && info.TargetKind == "machine" {
		pathDone := make(chan struct{})
		go func() { defer close(pathDone); watchMachineTransportPath(ctx, d.peerLocal, info.MachineID, bar) }()
		defer func() { cancelConnection(); <-pathDone }()
	}
	var inboxWorker terminalInboxWorker
	stopInbox := inboxWorker.stop
	startInbox := func(client *filetransfer.Client, sessionID string) {
		stopInbox()
		if client == nil || sessionID == "" {
			return
		}
		notify := func(message string) {
			if useStatusBar {
				bar.Notice(message)
				return
			}
			fmt.Fprintln(os.Stderr, message)
		}
		inboxPath, pathErr := configuredInboxPath()
		if pathErr != nil {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", pathErr); reference != "" {
				notify("File delivery is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
			return
		}
		machineID, machineErr := configuredMachineID()
		if machineErr != nil {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", machineErr); reference != "" {
				notify("File delivery is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
			return
		}
		inboxConfig := inbox.Config{Client: client, MachineID: machineID, SessionID: sessionID, Path: inboxPath, Notify: notify}
		receiver, inboxErr := inbox.New(inboxConfig)
		if inboxErr != nil {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", inboxErr); reference != "" {
				notify("File delivery is unavailable. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
			return
		}
		inboxWorker.start(ctx, receiver.Run, func(err error) {
			if reference := reportTerminalAuxFailure(ctx, "delivery", "file_transfer_failed", err); reference != "" {
				notify("File delivery stopped. Check `pb inbox` and `pb doctor`, then reconnect. Support reference: " + reference + ".")
			}
		})
	}
	defer stopInbox()
	var pastePolicy *paste.Policy
	conn = tunnel.NewObservedReconnectingConn(ctx, conn, d.cfg.Connect.DialRetries, time.Duration(d.cfg.Connect.DialRetrySeconds)*time.Second, func(reconnectCtx context.Context) (tunnel.Conn, error) {
		freshCred, credErr := d.auth.Credential()
		if credErr != nil {
			return nil, credErr
		}
		freshResolver, resolverErr := newResolver(freshCred)
		if resolverErr != nil {
			return nil, resolverErr
		}
		reconnectRequest := resolver.ConnectRequest{Machine: info.MachineID, Credential: freshCred, TerminalSessionID: terminalSessionID}
		if target.kind == environmentUserMachine && info.MachineGeneration > 0 {
			reconnectRequest.ResolvedMachine = &resolver.ResolvedMachine{ID: info.MachineID, Name: info.Machine, State: info.MachineState, Generation: info.MachineGeneration}
		}
		freshInfo, resolveErr := freshResolver.Resolve(reconnectCtx, reconnectRequest)
		if resolveErr != nil {
			var apiErr *api.APIError
			if errors.As(resolveErr, &apiErr) && apiErr.Code == "machine_revoked" {
				resolveErr = tunnel.StopReconnect(resolveErr)
			}
			return nil, resolveErr
		}
		if freshInfo.Terminal != nil {
			freshInfo.Terminal.Debug = c.Bool("debug")
			freshInfo.Terminal.RestartIfNotRunning = false
			freshInfo.Terminal.ReplayHistory = false
			freshInfo.Terminal.AfterSequence = int(lastTerminalSequence.Load())
			freshInfo.Terminal.SequenceSink = recordTerminalSequence
			freshInfo.Terminal.ReplayGapSink = recordReplayGap
			freshInfo.Terminal.Env = forwardedTerminalEnv(config.TerminalEnv)
			freshInfo.Terminal.Cols, freshInfo.Terminal.Rows = remoteSize()
		}
		freshConn, dialErr := d.tunnel.Dial(reconnectCtx, freshInfo)
		if dialErr != nil {
			if !tunnel.FallbackEligible(dialErr) {
				return nil, tunnel.StopReconnect(dialErr)
			}
			return nil, dialErr
		}
		if c.Bool("debug") {
			bar.SetDebugVersions(buildinfo.Version, tunnel.TerminalRuntimeVersion(freshConn))
		}
		if pastePolicy != nil {
			var freshTransfer *filetransfer.NativeClient
			var freshLease *localapi.FileTransferLease
			if freshInfo.FileTransfer != nil && d.peerLocal != nil {
				var transferErr error
				freshTransfer, freshLease, transferErr = nativeFileTransferClient(reconnectCtx, d.peerLocal, freshInfo, newIdempotencyKey())
				observeTransferFailure(transferErr)
				if freshTransfer != nil {
					if policyErr := freshTransfer.VerifyPolicy(reconnectCtx, descriptorFileTransferPolicy(freshInfo.FileTransfer)); policyErr != nil {
						observeTransferFailure(policyErr)
						observeTransferFailure(freshLease.Close())
						freshTransfer, freshLease = nil, nil
					}
				}
			}
			stopInbox()
			oldLease := transferLease
			transferClient, transferLease = freshTransfer, freshLease
			var freshUploader paste.BatchUploader = freshTransfer
			pastePolicy.Update(freshUploader, freshInfo.Terminal.SessionID, fileTransferLimits(freshInfo.FileTransfer))
			if oldLease != nil {
				observeTransferFailure(oldLease.Close())
			}
			if freshTransfer != nil {
				startInbox(freshTransfer.Client, freshInfo.Terminal.SessionID)
			} else {
				startInbox(nil, freshInfo.Terminal.SessionID)
			}
		}
		return freshConn, nil
	}, d.telemetry, nil, tunnel.TelemetryContext{MachineID: info.MachineID, EnvironmentID: info.Terminal.EnvironmentID}, tunnel.WithReconnectingOutput(
		d.cfg.Connect.TerminalOutputQueueChunks,
		time.Duration(d.cfg.Connect.TerminalOutputBatchMilliseconds)*time.Millisecond,
	), tunnel.WithReconnectObserver(func(event tunnel.ReconnectEvent) {
		if event == tunnel.ReconnectRecovered {
			bar.ResetRemoteState()
			cols, rows := remoteSize()
			forceTerminalRedraw(conn, rows, cols)
		}
		if !useStatusBar {
			return
		}
		switch event {
		case tunnel.ReconnectStarted:
			bar.SetConnection("reconnecting")
			bar.Loading("Reconnecting")
		case tunnel.ReconnectRecovered:
			bar.RecoverFailureFor("connection")
			bar.SetConnection("connected")
			bar.Notice("Reconnected")
		case tunnel.ReconnectFailed:
			bar.SetConnection("failed")
			bar.FailureFor("connection", "Connection lost")
		}
	}))

	// Wrap remote input with the file-paste interceptor.
	var pasteUploader paste.BatchUploader = transferClient
	pastePolicy = paste.NewPolicy(pasteUploader, info.Terminal.SessionID, fileTransferLimits(info.FileTransfer))
	interceptor := paste.NewWithPolicy(conn, pastePolicy,
		paste.WithDirectInput(),
		paste.WithNotifier(statusNotifier(useStatusBar)),
		paste.WithLifecycle(func(event paste.LifecycleEvent) {
			if !useStatusBar {
				return
			}
			switch event {
			case paste.FileDetected:
				bar.LoadingPersistent("Preparing file")
			case paste.FileUploading:
				bar.LoadingPersistent("Uploading file")
			case paste.FileComplete:
				bar.RecoverFailureFor("upload")
				bar.Notice("File uploaded")
			case paste.FileFailed:
				bar.FailureFor("upload", "File upload failed; pasted original")
			}
		}),
		paste.WithWatchDirs(expandDirs(d.cfg.FilePaste.WatchDirs)),
		paste.WithTempFilePatterns(d.cfg.FilePaste.TempFilePatterns),
		paste.WithMaxQueuedBytes(d.cfg.FilePaste.MaxQueuedInputBytes),
		paste.WithPartialFlushDelay(time.Duration(d.cfg.Connect.InputPartialFlushMilliseconds)*time.Millisecond),
	)
	if transferClient != nil {
		startInbox(transferClient.Client, info.Terminal.SessionID)
	}

	runOptions := []session.RunOption{
		session.WithOutputBufferBytes(d.cfg.Connect.TerminalOutputBufferBytes),
		session.WithBracketedPaste(),
	}
	if useStatusBar {
		bar.SetViewportChanged(func(cols, rows uint16) {
			if cols > 0 && rows > 0 {
				_ = conn.Resize(rows, cols)
			}
		})
		runOptions = append(runOptions, session.WithOutput(bar), session.WithRemoteSize(remoteSize))
	}
	code, err := session.Run(ctx, conn, interceptor, runOptions...)
	if err == nil {
		if useStatusBar {
			bar.ClearForExit()
		} else if term.IsTerminal(int(os.Stdout.Fd())) {
			_, _ = fmt.Fprint(os.Stdout, "\x1b[r\x1b[2J\x1b[H")
		}
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCodeError{code: code}
	}
	return nil
}

// watchMachineTransportPath keeps the session marker in sync with the observed
// machine path. A mixed machine snapshot cannot identify this session's path.
type machineTransportWatcher interface {
	Snapshot(context.Context) (localapi.Snapshot, error)
	Watch(context.Context, uint64) (<-chan localapi.Snapshot, <-chan error)
}

func watchMachineTransportPath(ctx context.Context, client machineTransportWatcher, machineID string, bar *statusbar.Bar) {
	if client == nil || bar == nil || machineID == "" {
		return
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		reportTerminalAuxFailure(ctx, "local_gateway", "native_private_failed", err)
		return
	}
	applyMachineTransportPath(snapshot, machineID, bar)
	updates, watchErr := client.Watch(ctx, snapshot.Generation)
	defer func() {
		// Both channels close after the API's stream reader and body cleanup.
		// Consume its final error even when cancellation or update EOF wins
		// the select; neither event proves there was no independent I/O fault.
		for err := range watchErr {
			reportTerminalAuxFailure(ctx, "local_gateway", "native_private_failed", err)
		}
		for range updates {
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			applyMachineTransportPath(snapshot, machineID, bar)
		case err := <-watchErr:
			if reportTerminalAuxFailure(ctx, "local_gateway", "native_private_failed", err) != "" && ctx.Err() == nil {
				bar.SetTransport("")
				bar.FailureFor("transport_status", "Connection path status unavailable")
			}
			return
		}
	}
}

func applyMachineTransportPath(snapshot localapi.Snapshot, machineID string, bar *statusbar.Bar) {
	for _, machine := range snapshot.Machines {
		if machine.ID == machineID && (machine.SelectedPath == "direct" || machine.SelectedPath == "relay" || machine.SelectedPath == "wss") {
			bar.SetTransport(machine.SelectedPath)
			return
		}
	}
}

func forceTerminalRedraw(conn tunnel.Conn, rows, cols uint16) {
	probeRows, probeCols := rows, cols
	if probeRows > 1 {
		probeRows--
	} else if probeCols > 1 {
		probeCols--
	}
	if probeRows != rows || probeCols != cols {
		_ = conn.Resize(probeRows, probeCols)
	}
	_ = conn.Resize(rows, cols)
}

func chooseIndex(ctx context.Context, title, subtitle string, count int, item func(int) selector.Item) (int, error) {
	items := make([]selector.Item, count)
	indexes := make(map[string]int, count)
	for index := range count {
		items[index] = item(index)
		if items[index].ID == "" {
			items[index].ID = strconv.Itoa(index)
		}
		indexes[items[index].ID] = index
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Title: title, Subtitle: subtitle, Items: items, Stdin: os.Stdin, Output: os.Stderr})
	return indexes[selected.ID], err
}

func statusNotifier(enabled bool) io.Writer {
	if enabled {
		return io.Discard
	}
	return os.Stderr
}

func connectTelemetry(cfg *config.Config, warnings io.Writer) (telemetry.Sink, func()) {
	if path := cfg.TelemetryPath(); path != "" {
		fileSink, err := telemetry.NewJSONFileSinkWithLimit(path, cfg.Observability.MaxEventLogBytes)
		if err == nil {
			return fileSink, func() {
				if closeErr := fileSink.Close(); closeErr != nil {
					fault := errorreport.Current().ObserveFailure(context.Background(), "pb", "diagnostic", "diagnostic_storage", "diagnostic_storage_unavailable", closeErr)
					fmt.Fprintf(warnings, "warning: local event log could not be saved; check its storage permissions. Support reference: %s.\n", fault.SupportReference)
				}
			}
		}
		fault := errorreport.Current().ObserveFailure(context.Background(), "pb", "diagnostic", "diagnostic_storage", "diagnostic_storage_unavailable", err)
		fmt.Fprintf(warnings, "warning: local event log unavailable; check its storage permissions. Support reference: %s.\n", fault.SupportReference)
	}
	return telemetry.NopSink{}, func() {}
}

func retryableInitialConnectError(err error) bool {
	return retryableInitialConnectFailure(err)
}

func friendlyAPIError(err error) string {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch apiErr.Code {
	case "team_subscription_required":
		return "An active team subscription is required. " + teamSubscriptionRecovery + " No resource was created."
	case "credits_exhausted":
		return "credits are exhausted; top up credits in Paperboat, then retry"
	case "entitlement_lost", "payment_required":
		return "your Paperboat plan is inactive; restore billing access, then retry"
	case "tunnel_unavailable":
		return "the secure tunnel is not available yet; retry in a moment"
	case "machine_not_ready":
		return "the machine is not ready yet; retry in a moment"
	case "machine_offline":
		return "the machine is offline; start or repair its Paperboat connector, then retry"
	case "machine_revoked":
		return "this machine has been disconnected or revoked; repair or reconnect it in the Paperboat dashboard"
	}
	return ""
}

var newTransferCommandClient = func(ctx context.Context, target *resolver.FileTransferTarget, destination api.UserMachine, operationID string) (*filetransfer.NativeClient, io.Closer, error) {
	if target == nil || target.Auth.Method != "bearer" || target.Auth.ResourceID == "" || destination.EnvironmentID == "" || destination.InstallationGeneration <= 0 || destination.ID != target.DestinationMachineID || operationID == "" {
		return nil, nil, errors.New("server returned an invalid file transfer descriptor")
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, nil, err
	}
	local, err := localapi.NewClient(paths.SocketPath, time.Duration(config.PeerConnectTimeoutMilliseconds)*time.Millisecond)
	if err != nil {
		return nil, nil, err
	}
	deadline := parseAuthExpiry(target.Auth.ExpiresAt)
	lease, err := local.PrepareFileTransfer(ctx, localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: destination.ID, EnvironmentID: destination.EnvironmentID, MachineGeneration: uint64(destination.InstallationGeneration), OperationID: operationID, Credential: target.Auth.Token, AccessSessionID: target.Auth.ResourceID, Deadline: deadline, MaximumBytes: uint64(target.Policy.MaxFileBytes)})
	if err != nil {
		return nil, nil, err
	}
	client, err := filetransfer.NewNativeClient(target.Endpoint, filetransfer.Auth{Token: target.Auth.Token, ExpiresAt: deadline}, filetransfer.Binding{SourceMachineID: target.SourceMachineID, DestinationMachineID: target.DestinationMachineID, InitiatingUserID: target.InitiatingUserID}, lease.OpenTransferStream)
	if err != nil {
		_ = lease.Close()
		return nil, nil, err
	}
	if target.Policy.DeliveryTimeoutSeconds > 0 {
		client.DeliveryTimeout = time.Duration(target.Policy.DeliveryTimeoutSeconds) * time.Second
	}
	return client, lease, nil
}

func localFileTransferSenderFromEnvironment() (*filetransfer.LocalSender, error) {
	endpoint := strings.TrimSpace(os.Getenv("PAPERBOAT_FILE_TRANSFER_STAGING_ENDPOINT"))
	tokenPath := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_AGENT_TOKEN_FILE"))
	if endpoint == "" && tokenPath == "" {
		return nil, nil
	}
	if endpoint == "" || !filepath.IsAbs(tokenPath) {
		return nil, errors.New("Paperboat host-local file transfer environment is invalid")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "/v1/local-file-transfers" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Paperboat host-local file transfer endpoint is invalid")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "" {
		return nil, errors.New("Paperboat host-local file transfer endpoint is not loopback")
	}
	token, err := readOwnerOnlyFile(tokenPath, 1024)
	if err != nil {
		return nil, fmt.Errorf("read Paperboat host-local file transfer token: %w", err)
	}
	value := strings.TrimSpace(string(token))
	clear(token)
	if len(value) < 32 || strings.ContainsAny(value, " \t\r\n") {
		return nil, errors.New("Paperboat host-local file transfer token is invalid")
	}
	return &filetransfer.LocalSender{Endpoint: endpoint, Token: value}, nil
}

func nativeFileTransferClient(ctx context.Context, local *localapi.Client, target resolver.ConnectInfo, operationID string) (*filetransfer.NativeClient, *localapi.FileTransferLease, error) {
	if ctx == nil || local == nil || target.TargetKind != "machine" || target.MachineID == "" || target.MachineGeneration == 0 || target.Terminal == nil || target.Terminal.EnvironmentID == "" || target.FileTransfer == nil || target.FileTransfer.Auth.ResourceID == "" || operationID == "" {
		return nil, nil, errors.New("native file transfer target is invalid")
	}
	deadline := parseAuthExpiry(target.FileTransfer.Auth.ExpiresAt)
	lease, err := local.PrepareFileTransfer(ctx, localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: target.MachineID, EnvironmentID: target.Terminal.EnvironmentID, MachineGeneration: target.MachineGeneration, OperationID: operationID, Credential: target.FileTransfer.Auth.Token, AccessSessionID: target.FileTransfer.Auth.ResourceID, Deadline: deadline, MaximumBytes: uint64(target.FileTransfer.Policy.MaxFileBytes)})
	if err != nil {
		return nil, nil, err
	}
	client, err := filetransfer.NewNativeClient(target.FileTransfer.Endpoint, filetransfer.Auth{Token: target.FileTransfer.Auth.Token, ExpiresAt: deadline}, filetransfer.Binding{SourceMachineID: target.FileTransfer.SourceMachineID, DestinationMachineID: target.FileTransfer.DestinationMachineID, InitiatingUserID: target.FileTransfer.InitiatingUserID}, lease.OpenTransferStream)
	if err != nil {
		_ = lease.Close()
		return nil, nil, err
	}
	if target.FileTransfer.Policy.DeliveryTimeoutSeconds > 0 {
		client.DeliveryTimeout = time.Duration(target.FileTransfer.Policy.DeliveryTimeoutSeconds) * time.Second
	}
	return client, lease, nil
}

func fileTransferLimits(target *resolver.FileTransferTarget) filetransfer.Limits {
	if target == nil {
		return filetransfer.Limits{MaxFileBytes: 50 << 20, MaxBatchFiles: 10, MaxBatchBytes: 500 << 20}
	}
	return filetransfer.Limits{MaxFileBytes: target.Policy.MaxFileBytes, MaxBatchFiles: target.Policy.MaxBatchFiles, MaxBatchBytes: target.Policy.MaxBatchBytes}
}

func descriptorFileTransferPolicy(target *resolver.FileTransferTarget) filetransfer.Policy {
	if target == nil {
		return filetransfer.Policy{}
	}
	policy := target.Policy
	return filetransfer.Policy{Revision: policy.Revision, MaxFileBytes: policy.MaxFileBytes, MaxBatchFiles: policy.MaxBatchFiles, MaxBatchBytes: policy.MaxBatchBytes, MaxConcurrentTransfers: policy.MaxConcurrentTransfers, RetentionSeconds: policy.RetentionSeconds, DeliveryTimeoutSeconds: policy.DeliveryTimeoutSeconds, MaxPendingSpoolBytes: policy.MaxPendingSpoolBytes}
}

func parseAuthExpiry(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

// terminalEnvKeyPattern mirrors the terminal RPC environment schema; an invalid
// key or oversized value would reject the whole attach, so filter locally.
var terminalEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const maxTerminalEnvValueChars = 8_192

// forwardedTerminalEnv snapshots the configured local environment variables so
// the remote PTY spawns with the client terminal's capabilities.
func forwardedTerminalEnv(keys []string) map[string]string {
	env := make(map[string]string, len(keys))
	for _, key := range keys {
		if !terminalEnvKeyPattern.MatchString(key) {
			continue
		}
		value, ok := os.LookupEnv(key)
		if !ok || value == "" || len(value) > maxTerminalEnvValueChars {
			continue
		}
		env[key] = value
	}
	return env
}

// localTerminalSize returns the current terminal geometry, clamped to the
// terminal RPC schema bounds, or zeros when stdout is not a terminal.
func localTerminalSize() (cols, rows uint16) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0
	}
	if w > 1000 {
		w = 1000
	}
	if h > 500 {
		h = 500
	}
	return uint16(w), uint16(h)
}

func expandDirs(dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if home != "" && len(d) >= 1 && d[0] == '~' {
			d = home + d[1:]
		}
		out = append(out, d)
	}
	return out
}

func configCommand() *command.Spec {
	return &command.Spec{
		Name:  "config",
		Usage: "Inspect the local CLI config",
		Subcommands: []*command.Spec{
			{Name: "status", ArgsUsage: "[environment]", Usage: "Show configuration synchronization status", Action: configStatus},
			{
				Name: "unassign", ArgsUsage: "<environment>", Usage: "Remove a config repository assignment",
				Action: configUnassign,
			},
			{Name: "approve", ArgsUsage: "<environment>", Usage: "Approve the currently reviewed pull revision", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: configApproveRevision},
			{Name: "team-default-set", ArgsUsage: "<team> <repository>", Usage: "Set a team's default pull repository", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: configTeamDefaultSet},
			{Name: "team-default-adopt", ArgsUsage: "<team>", Usage: "Adopt a team default using your provider access", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: configTeamDefaultAdopt},
			{Name: "team-default-unadopt", Usage: "Stop inheriting a team configuration default", Flags: []command.Flag{&command.BoolFlag{Name: "json"}}, Action: configTeamDefaultUnadopt},
			{
				Name: "set", ArgsUsage: "<key> <value>", Usage: "Set a local configuration value",
				Flags: []command.Flag{&command.BoolFlag{Name: "json"}},
				Action: func(c *command.Context) error {
					if c.Args().Len() != 2 {
						return localArgumentError("usage: pb config set server <url>")
					}
					cfg, err := config.Load(c.String("config"))
					if err != nil {
						return err
					}
					switch c.Args().First() {
					case "server":
						server, err := config.NormalizeServerURL(c.Args().Get(1))
						if err != nil {
							return usageError{err: err, publicMessage: "The server URL is invalid. Use an absolute HTTP or HTTPS URL without credentials, a query or a fragment."}
						}
						cfg.ServerURL = server
					case "ssh-target-port":
						port, parseErr := strconv.ParseUint(c.Args().Get(1), 10, 16)
						if parseErr != nil || port == 0 {
							return localArgumentError("ssh-target-port must be between 1 and 65535")
						}
						return configSetSSHTargetPort(c, uint16(port))
					default:
						return localArgumentError("usage: pb config set server <url> or pb config set ssh-target-port <1-65535>")
					}
					if err := cfg.Save(); err != nil {
						return err
					}
					if c.Bool("json") {
						return writeCLIJSON(c.Writer, map[string]any{"path": cfg.Path(), "key": c.Args().First(), "updated": true})
					}
					fmt.Fprintln(c.Writer, cfg.Path())
					return nil
				},
			},
			{
				Name: "unset", ArgsUsage: "server", Usage: "Remove a local configuration value",
				Flags: []command.Flag{&command.BoolFlag{Name: "json"}},
				Action: func(c *command.Context) error {
					if c.Args().Len() != 1 || c.Args().First() != "server" {
						return localArgumentError("usage: pb config unset server")
					}
					cfg, err := config.Load(c.String("config"))
					if err != nil {
						return err
					}
					cfg.ServerURL = ""
					if err := cfg.Save(); err != nil {
						return err
					}
					if c.Bool("json") {
						return writeCLIJSON(c.Writer, map[string]any{"path": cfg.Path(), "key": "server", "unset": true})
					}
					fmt.Fprintln(c.Writer, cfg.Path())
					return nil
				},
			},
			{
				Name:  "path",
				Usage: "Print the config file path",
				Flags: []command.Flag{&command.BoolFlag{Name: "json"}},
				Action: func(c *command.Context) error {
					d, err := buildDeps(c)
					if err != nil {
						return err
					}
					if c.Bool("json") {
						return writeCLIJSON(c.Writer, map[string]any{"path": d.cfg.Path()})
					}
					fmt.Fprintln(c.Writer, d.cfg.Path())
					return nil
				},
			},
			{
				Name:  "show",
				Usage: "Print the effective config",
				Flags: []command.Flag{&command.BoolFlag{Name: "json"}},
				Action: func(c *command.Context) error {
					d, err := buildDeps(c)
					if err != nil {
						return err
					}
					if c.Bool("json") {
						return json.NewEncoder(c.Writer).Encode(map[string]any{"path": d.cfg.Path(), "server_url": d.cfg.ServerURL, "auth_file_fallback": d.cfg.Auth.AllowFileFallback, "file_paste": d.cfg.FilePaste, "status_bar": d.cfg.StatusBar, "local_access": d.cfg.LocalAccess})
					}
					fmt.Fprintf(c.Writer, "server_url: %s\n", orNone(d.cfg.ServerURL))
					fmt.Fprintf(c.Writer, "auth.file_fallback: %t\n", d.cfg.Auth.AllowFileFallback)
					fmt.Fprintf(c.Writer, "file_paste.max_queued_input_bytes: %d\n", d.cfg.FilePaste.MaxQueuedInputBytes)
					fmt.Fprintf(c.Writer, "status_bar.mode: %s\n", d.cfg.StatusBar.Mode)
					fmt.Fprintf(c.Writer, "status_bar.fullscreen: %s\n", d.cfg.StatusBar.Fullscreen)
					fmt.Fprintf(c.Writer, "status_bar.theme: %s\n", d.cfg.StatusBar.Theme)
					fmt.Fprintf(c.Writer, "local_access.domain: %s\n", d.cfg.LocalAccess.Domain)
					fmt.Fprintln(c.Writer, "local_access.service_aliases:")
					if len(d.cfg.LocalAccess.ServiceAliases) == 0 {
						fmt.Fprintln(c.Writer, "  (none)")
					} else {
						for _, alias := range d.cfg.LocalAccess.ServiceAliases {
							fmt.Fprintf(c.Writer, "  %s.%s -> configured port %d\n", alias.Name, alias.MachineAlias, alias.Port)
						}
					}
					return nil
				},
			},
		},
	}
}

func configSetSSHTargetPort(c *command.Context, port uint16) error {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return err
		}
	}
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil || registration.InstallationGeneration < 1 {
		return commandPreparationFailure{step: prepareMachineRegistration, cause: err}
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	generation := uint64(registration.InstallationGeneration)
	current, err := client.ManagedSSHTarget(c.Context, registration.MachineID, generation)
	if err != nil {
		return err
	}
	if current.Port != port {
		if _, err := client.UpdateManagedSSHTargetPort(c.Context, registration.MachineID, generation, current.ReconciliationVersion, port, newIdempotencyKey()); err != nil {
			return err
		}
	}
	registration.SSHPort = port
	registration.UpdatedAt = time.Now().UTC()
	if err := store.SaveRegistration(registration); err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"key": "ssh-target-port", "port": port, "updated": true})
	}
	fmt.Fprintf(c.Writer, "SSH target port set to %d\n", port)
	return nil
}

func statusBarConfigCommand() *cobra.Command {
	status := &cobra.Command{
		Use:   "status-bar",
		Short: "Configure the interactive terminal status bar",
		Args:  commandArgs(cobra.NoArgs),
		RunE:  func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	show := &cobra.Command{Use: "show", Short: "Show the effective status-bar configuration", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		jsonOutput, _ := command.Flags().GetBool("json")
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(cfg.StatusBar)
		}
		return printStatusBarConfig(command.OutOrStdout(), cfg.StatusBar)
	}}
	show.Flags().Bool("json", false, "print JSON")
	set := &cobra.Command{Use: "set <key> <value>", Short: "Set a status-bar preference", Args: commandArgs(cobra.ExactArgs(2)), RunE: func(command *cobra.Command, args []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		if err := setStatusBarValue(&cfg.StatusBar, args[0], args[1]); err != nil {
			return invocationError(err)
		}
		if err := cfg.Validate(); err != nil {
			return invocationError(err)
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"path": cfg.Path(), "status_bar": cfg.StatusBar})
		}
		fmt.Fprintln(command.OutOrStdout(), cfg.Path())
		return nil
	}}
	set.Flags().Bool("json", false, "print JSON")
	reset := &cobra.Command{Use: "reset", Short: "Restore status-bar defaults", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		cfg.StatusBar = config.DefaultStatusBarConfig()
		if err := cfg.Save(); err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"path": cfg.Path(), "status_bar": cfg.StatusBar})
		}
		fmt.Fprintln(command.OutOrStdout(), cfg.Path())
		return nil
	}}
	reset.Flags().Bool("json", false, "print JSON")
	preview := &cobra.Command{Use: "preview", Short: "Preview the configured status bar", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := config.Load(configPathFlag(command))
		if err != nil {
			return err
		}
		width, _ := command.Flags().GetInt("width")
		if width == 0 {
			width = 80
			if term.IsTerminal(int(os.Stdout.Fd())) {
				if detected, _, sizeErr := term.GetSize(int(os.Stdout.Fd())); sizeErr == nil {
					width = detected
				}
			}
		}
		bar := newConfiguredStatusBar(cfg.StatusBar, statusbar.ModeOff)
		defer bar.Close()
		bar.SetIdentity("paperboat", "default")
		bar.SetUsage("100", "12 GB")
		bar.SetConfigSync("healthy")
		bar.SetConnection("connected")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		bar.SetConnection("reconnecting")
		bar.Loading("Reconnecting")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		bar.SetConnection("failed")
		bar.FailureFor("preview", "Connection lost")
		fmt.Fprintln(command.OutOrStdout(), bar.Render(width))
		return nil
	}}
	preview.Flags().Int("width", 0, "preview width (20-500 columns)")
	preview.PreRunE = func(command *cobra.Command, _ []string) error {
		width, _ := command.Flags().GetInt("width")
		if width != 0 && (width < 20 || width > 500) {
			return invocationError(errors.New("--width must be between 20 and 500"))
		}
		return nil
	}
	status.AddCommand(show, set, reset, preview)
	return status
}

func configPathFlag(command *cobra.Command) string {
	value, _ := command.Flags().GetString("config")
	return value
}

func printStatusBarConfig(writer io.Writer, value config.StatusBarConfig) error {
	_, err := fmt.Fprintf(writer, "mode: %s\nfullscreen: %s\ntheme: %s\nprivacy: %t\nterminal_title: %t\nnotice_seconds: %d\nleft: %s\ncenter: %s\nright: %s\nforeground: %s\nbackground: %s\naccent: %s\nwarning: %s\nerror: %s\n",
		value.Mode, value.Fullscreen, value.Theme, value.Privacy, value.TerminalTitle, value.NoticeSeconds,
		formatWidgetList(value.Left), formatWidgetList(value.Center), formatWidgetList(value.Right),
		inheritedColor(value.Colors.Foreground), inheritedColor(value.Colors.Background), inheritedColor(value.Colors.Accent), inheritedColor(value.Colors.Warning), inheritedColor(value.Colors.Error))
	return err
}

func inheritedColor(value string) string {
	if value == "" {
		return "inherit"
	}
	return value
}

func formatWidgetList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}

func setStatusBarValue(value *config.StatusBarConfig, key, raw string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	raw = strings.TrimSpace(raw)
	switch key {
	case "mode":
		value.Mode = strings.ToLower(raw)
	case "fullscreen":
		value.Fullscreen = strings.ToLower(raw)
	case "theme":
		value.Theme = strings.ToLower(raw)
	case "privacy", "terminal-title":
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%s must be true or false", key)
		}
		if key == "privacy" {
			value.Privacy = parsed
		} else {
			value.TerminalTitle = parsed
		}
	case "notice-seconds":
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 60 {
			return localArgumentError("notice-seconds must be between 1 and 60")
		}
		value.NoticeSeconds = parsed
	case "left", "center", "right":
		widgets := parseWidgetList(raw)
		switch key {
		case "left":
			value.Left = widgets
		case "center":
			value.Center = widgets
		case "right":
			value.Right = widgets
		}
	case "foreground", "background", "accent", "warning", "error":
		switch key {
		case "foreground":
			value.Colors.Foreground = raw
		case "background":
			value.Colors.Background = raw
		case "accent":
			value.Colors.Accent = raw
		case "warning":
			value.Colors.Warning = raw
		case "error":
			value.Colors.Error = raw
		}
	default:
		return fmt.Errorf("unknown status-bar key %q", key)
	}
	return nil
}

func parseWidgetList(raw string) []string {
	if raw == "" || strings.EqualFold(raw, "none") {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, strings.ToLower(strings.TrimSpace(part)))
	}
	return result
}

func newConfiguredStatusBar(value config.StatusBarConfig, mode string) *statusbar.Bar {
	return statusbar.New(statusbar.Options{
		Mode: mode, Fullscreen: value.Fullscreen, Theme: value.Theme, Privacy: value.Privacy, TerminalTitle: false,
		Colors:         statusbar.Colors{Foreground: value.Colors.Foreground, Background: value.Colors.Background, Accent: value.Colors.Accent, Warning: value.Colors.Warning, Error: value.Colors.Error},
		NoticeDuration: time.Duration(value.NoticeSeconds) * time.Second,
		Layout:         statusbar.Layout{Left: value.Left, Center: value.Center, Right: value.Right},
	})
}

func configUnassign(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	machineID := target.id
	assignment, err := client.ConfigAssignment(c.Context, machineID)
	if err != nil {
		return friendlyCommandError(err)
	}
	if err := confirmContextMutation(c, fmt.Sprintf("config-unassign:%s:%d", target.id, assignment.Version), fmt.Sprintf("Remove the config repository assignment from %s (%s)? The repository and its content will remain.", target.name, target.id)); err != nil {
		return err
	}
	if err := manageConfigService(c.Context, machineID, false); err != nil {
		return fmt.Errorf("stop config sync service: %w", err)
	}
	if err := client.UnassignConfig(c.Context, machineID, assignment.Version); err != nil {
		repairErr := manageConfigService(c.Context, machineID, true)
		return errors.Join(friendlyCommandError(err), repairErr)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "state": "unassigned", "outcome": "confirmed"})
	}
	fmt.Fprintf(c.Writer, "Removed config assignment from %s.\n", target.name)
	return nil
}

func configApproveRevision(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, target.id)
	if err != nil || len(items) != 1 {
		return errors.Join(errors.New("configuration revision is unavailable"), err)
	}
	item := items[0]
	if item.RemoteRevision == "" || item.AssignmentVersion < 1 {
		return errors.New("no reviewed configuration revision is pending")
	}
	if len(item.Review) > 0 && !c.Bool("json") {
		fmt.Fprintf(c.Writer, "Revision %s changes:\n", item.RemoteRevision)
		for _, change := range item.Review {
			fmt.Fprintf(c.Writer, "  %s  %s\n", change.Reason, change.Path)
		}
	}
	approved, err := client.ApproveConfigPullRevision(c.Context, target.id, item.RemoteRevision, item.AssignmentVersion)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "revision": item.RemoteRevision, "assignment": approved})
	}
	fmt.Fprintf(c.Writer, "Approved configuration revision %s for %s (assignment version %d).\n", item.RemoteRevision, target.name, approved.Version)
	return nil
}

func configTeamDefaultSet(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	repositories, err := client.ListConfigRepositories(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	repository, err := resolveConfigRepository(repositories, c.Args().Get(1))
	if err != nil {
		return err
	}
	version := int64(0)
	if current, getErr := client.ConfigTeamDefault(c.Context, c.Args().First()); getErr == nil {
		version = current.Version
	} else if !api.IsNotFound(getErr) {
		return friendlyCommandError(getErr)
	}
	item, err := client.SetConfigTeamDefault(c.Context, c.Args().First(), repository.ID, version)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, item)
	}
	fmt.Fprintf(c.Writer, "Team %s default pull repository is %s (version %d).\n", item.TeamID, item.DisplayName, item.Version)
	return nil
}

func configTeamDefaultAdopt(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	item, err := client.ConfigTeamDefault(c.Context, c.Args().First())
	if err != nil {
		return friendlyCommandError(err)
	}
	adoption, err := client.AdoptConfigTeamDefault(c.Context, item.TeamID, item.Version)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, adoption)
	}
	fmt.Fprintf(c.Writer, "Adopted %s from team %s at version %d; personal machine assignments still take precedence.\n", adoption.DisplayName, adoption.TeamID, adoption.AdoptedVersion)
	return nil
}

func configTeamDefaultUnadopt(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	if err := client.UnadoptConfigTeamDefault(c.Context); err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"adopted": false})
	}
	fmt.Fprintln(c.Writer, "Stopped inheriting the team configuration default; personal assignments were unchanged.")
	return nil
}

func manageConfigService(ctx context.Context, machineID string, install bool) error {
	stateRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "machine-registration.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return err
	}
	registration, err := identityStore.Registration()
	if err != nil {
		return err
	}
	if registration.MachineID != machineID {
		return nil
	}
	if handled, windowsErr := manageWindowsConfigService(ctx, stateRoot, install); handled {
		return windowsErr
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if install {
		executable, err = endpointbinary.Daemon(executable)
	} else {
		executable, err = endpointbinary.DaemonPathForRemoval(executable)
	}
	if err != nil {
		return err
	}
	account, err := user.Current()
	if err != nil {
		return err
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return err
	}
	query := &exec.Cmd{}
	prepareConfigServiceQuery(query)
	runner := service.ExecRunner{Environment: query.Env}
	var controller service.Controller
	switch runtime.GOOS {
	case "darwin":
		uid, parseErr := strconv.Atoi(account.Uid)
		if parseErr != nil {
			return parseErr
		}
		controller = service.LaunchdController{Runner: runner, UID: uid, Label: service.ConfigLabel, UserDomain: true}
	case "linux":
		controller = service.SystemdController{Runner: runner, Unit: "paperboat-runtime-config.service", User: true}
	default:
		return service.ErrUnsupportedPlatform
	}
	definition := service.Config{Platform: runtime.GOOS, Kind: service.ConfigKind, ConfigRoot: home, Executable: executable, User: account.Username, Group: group.Name, Arguments: []string{"daemon", "__runtime-config", "--state-root", stateRoot}, Environment: map[string]string{"HOME": home}, Controller: controller}
	if !install {
		return service.Remove(ctx, definition)
	}
	installer, err := service.New(definition)
	if err != nil {
		return err
	}
	return installer.Install(ctx)
}

func configStatus(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().First())
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "state": status.State, "environments": items})
	}
	if len(items) == 0 {
		fmt.Fprintln(c.Writer, "No configuration assignments are reporting status.")
		return nil
	}
	for _, item := range items {
		name := item.Alias
		if name == "" {
			name = item.EnvironmentID
		}
		fmt.Fprintf(c.Writer, "%s: %s, %s, manifest %s, %d managed, %d pending clean\n",
			name, item.State, strings.ReplaceAll(item.Mode, "_", "-"), item.ManifestHealth,
			item.ManagedPathCount, item.PendingCleanPathCount)
		switch item.ErrorCode {
		case "configuration_changed":
			fmt.Fprintln(c.Writer, "  Configuration changed; run pb config sync apply on this machine to approve it.")
		case "configuration_invalid":
			fmt.Fprintln(c.Writer, "  Fix this machine's TOML file, then run pb config sync validate and pb config sync apply.")
		}
	}
	return nil
}

func configConflictCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "conflict", Short: "Inspect and resolve configuration conflicts"}
	list := &cobra.Command{Use: "list [environment]", Short: "List current path conflicts", Args: commandArgs(cobra.MaximumNArgs(1)), RunE: actionRun(configConflictList)}
	list.Flags().Bool("json", false, "print JSON")
	show := &cobra.Command{Use: "show <environment> <path>", Short: "Show a current path conflict", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictShow)}
	show.Flags().Bool("json", false, "print JSON")
	resolve := &cobra.Command{Use: "resolve <environment> <path>", Short: "Choose the machine or repository version", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictResolve)}
	resolve.Flags().String("keep", "", "version to keep: machine or repository")
	resolve.Flags().Bool("json", false, "print JSON")
	compare := &cobra.Command{Use: "compare <environment> <path>", Short: "Read current machine and repository conflict contents through an encrypted connection", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictCompare)}
	compare.Flags().Bool("json", false, "print comparison content as base64 JSON")
	root.AddCommand(list, show, compare, resolve)
	return root
}

func configForceCobraCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "force <pull|push> <environment> [path]", Short: "Force a scoped configuration direction",
		Args: commandArgs(cobra.RangeArgs(2, 3)), RunE: actionRun(configForce),
	}
	command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func configConflictList(c *command.Context) error {
	status, items, err := loadSelectedConfigStatus(c, c.Args().First())
	if err != nil {
		return err
	}
	type listedConflict struct {
		EnvironmentID   string                    `json:"environment_id"`
		EnvironmentName string                    `json:"environment_name"`
		Conflict        api.ConfigSyncPathSummary `json:"conflict"`
	}
	conflicts := make([]listedConflict, 0)
	for _, item := range items {
		for _, conflict := range item.Conflicts {
			conflicts = append(conflicts, listedConflict{item.EnvironmentID, item.Alias, conflict})
		}
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "state": status.State, "conflicts": conflicts})
	}
	if len(conflicts) == 0 {
		fmt.Fprintln(c.Writer, "No configuration conflicts.")
		return nil
	}
	for _, item := range conflicts {
		name := item.EnvironmentName
		if name == "" {
			name = item.EnvironmentID
		}
		fmt.Fprintf(c.Writer, "%s\t%s\t%s\n", name, item.Conflict.Path, strings.ReplaceAll(item.Conflict.Reason, "_", " "))
	}
	return nil
}

func configConflictShow(c *command.Context) error {
	_, items, err := loadSelectedConfigStatus(c, c.Args().First())
	if err != nil {
		return err
	}
	conflict, err := findConfigConflict(items[0], c.Args().Get(1))
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": items[0], "conflict": conflict})
	}
	fmt.Fprintf(c.Writer, "%s on %s\nReason: %s\nChoices: keep this machine's version or use repository version.\n",
		conflict.Path, items[0].Alias, strings.ReplaceAll(conflict.Reason, "_", " "))
	return nil
}

func configConflictResolve(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().First())
	if err != nil {
		return err
	}
	conflict, err := findConfigConflict(items[0], c.Args().Get(1))
	if err != nil {
		return err
	}
	action := ""
	switch strings.ToLower(strings.TrimSpace(c.String("keep"))) {
	case "machine", "local":
		action = "keep_local"
	case "repository", "remote":
		action = "keep_remote"
	default:
		return localArgumentError("config conflict resolve --keep must be machine or repository")
	}
	operation, err := client.ResolveConfigConflict(c.Context, items[0].EnvironmentID, api.ConfigConflictRequest{
		Path: conflict.Path, ConflictRevision: conflict.Revision, ExpectedRemoteRevision: items[0].RemoteRevision,
		ExpectedAssignmentVersion: items[0].AssignmentVersion, Action: action,
	})
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "operation": operation, "outcome": "queued"})
	}
	fmt.Fprintf(c.Writer, "Queued %s for %s on %s.\n", strings.ReplaceAll(action, "_", " "), conflict.Path, items[0].Alias)
	return nil
}

func configForce(c *command.Context) error {
	direction := strings.ToLower(strings.TrimSpace(c.Args().First()))
	if direction != "pull" && direction != "push" {
		return localArgumentError("config force direction must be pull or push")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().Get(1))
	if err != nil {
		return err
	}
	item := items[0]
	request := api.ConfigForceRequest{
		Scope: "config", ExpectedRemoteRevision: item.RemoteRevision, ExpectedAssignmentVersion: item.AssignmentVersion,
		Action: "force_" + direction, Confirmation: "FORCE " + strings.ToUpper(direction),
	}
	if c.Args().Len() == 3 {
		conflict, findErr := findConfigConflict(item, c.Args().Get(2))
		if findErr != nil {
			return findErr
		}
		request.Scope, request.Path, request.ConflictRevision = "path", conflict.Path, conflict.Revision
	}
	if err := confirmContextMutation(c, "config-force:"+item.EnvironmentID+":"+direction+":"+request.Scope+":"+request.Path+":"+request.ConflictRevision, fmt.Sprintf("Force %s for %s scope on %s (%s)? This queues a recoverable config sync operation.", direction, request.Scope, item.Alias, item.EnvironmentID)); err != nil {
		return err
	}
	operation, err := client.ForceConfig(c.Context, item.EnvironmentID, request)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "operation": operation, "outcome": "queued"})
	}
	fmt.Fprintf(c.Writer, "Queued force %s for %s on %s.\n", direction, request.Scope, item.Alias)
	return nil
}

func loadSelectedConfigStatus(c *command.Context, requested string) (api.ConfigSyncStatus, []api.ConfigSyncEnvironmentState, error) {
	client, err := backendClient(c)
	if err != nil {
		return api.ConfigSyncStatus{}, nil, err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return api.ConfigSyncStatus{}, nil, friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, requested)
	return status, items, err
}

func selectConfigEnvironments(items []api.ConfigSyncEnvironmentState, requested string) ([]api.ConfigSyncEnvironmentState, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return items, nil
	}
	matches := make([]api.ConfigSyncEnvironmentState, 0, 1)
	for _, item := range items {
		if item.EnvironmentID == requested || item.MachineID == requested || strings.EqualFold(item.Alias, requested) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 1 {
		return matches, nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("configuration environment %q is ambiguous; use its stable ID", requested)
	}
	return nil, fmt.Errorf("configuration environment %q was not found", requested)
}

func findConfigConflict(item api.ConfigSyncEnvironmentState, requested string) (api.ConfigSyncPathSummary, error) {
	for _, conflict := range item.Conflicts {
		if conflict.Path == requested {
			return conflict, nil
		}
	}
	return api.ConfigSyncPathSummary{}, fmt.Errorf("configuration conflict %q was not found on %s", requested, item.Alias)
}

func resolveConfigRepository(items []api.ConfigRepository, requested string) (api.ConfigRepository, error) {
	for _, item := range items {
		if item.ID == requested {
			return item, nil
		}
	}
	matches := make([]api.ConfigRepository, 0, 1)
	for _, item := range items {
		if strings.EqualFold(item.DisplayName, requested) || strings.EqualFold(item.ExternalRef, requested) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return api.ConfigRepository{}, fmt.Errorf("config repository %q is ambiguous; use a stable repository ID", requested)
	}
	return api.ConfigRepository{}, fmt.Errorf("config repository %q was not found", requested)
}

type proxyDoctorDiagnosis struct {
	State    string `json:"state"`
	Recovery string `json:"recovery"`
}

func doctorProxyDiagnosis(err error) (proxyDoctorDiagnosis, bool) {
	var proxyErr *httptransport.ProxyError
	if !errors.As(err, &proxyErr) {
		return proxyDoctorDiagnosis{}, false
	}
	switch proxyErr.Failure {
	case httptransport.ProxyAutomaticConfigurationUnsupported:
		return proxyDoctorDiagnosis{
			State:    "pac_unsupported",
			Recovery: "This network requires PAC/WPAD, which Paperboat does not execute. Configure a credential-free explicit HTTPS proxy with HTTPS_PROXY (or PAPERBOAT_HTTPS_PROXY for a managed service), then retry `pb doctor`.",
		}, true
	case httptransport.ProxyAuthenticationRequired:
		return proxyDoctorDiagnosis{
			State:    "authentication_required",
			Recovery: "The configured proxy requires authentication, which Paperboat does not accept in proxy URLs. Configure a credential-free explicit proxy, then retry `pb doctor`.",
		}, true
	case httptransport.ProxyInvalid:
		return proxyDoctorDiagnosis{
			State:    "invalid_configuration",
			Recovery: "Configure a credential-free http:// or https:// proxy URL with no path, query, or fragment, then retry `pb doctor`.",
		}, true
	default:
		return proxyDoctorDiagnosis{}, false
	}
}

type localDoctorReport struct {
	failure                error
	StateRoot              string   `json:"state_root,omitempty"`
	SetupState             string   `json:"setup_state"`
	MachineID              string   `json:"machine_id,omitempty"`
	EnvironmentID          string   `json:"environment_id,omitempty"`
	InstallationGeneration int64    `json:"installation_generation,omitempty"`
	IdentityState          string   `json:"identity_state"`
	CredentialState        string   `json:"machine_control_credential"`
	InboxPath              string   `json:"inbox_path,omitempty"`
	InboxState             string   `json:"inbox_state"`
	ConfigService          string   `json:"config_service"`
	HostRuntime            string   `json:"host_runtime"`
	TrackedSessions        uint64   `json:"tracked_sessions"`
	ActiveProcesses        uint64   `json:"active_processes"`
	ActiveAttachments      uint64   `json:"active_attachments"`
	ActiveUploads          uint64   `json:"active_uploads"`
	WorkloadCounts         string   `json:"workload_counts_state"`
	RecoveryActions        []string `json:"recovery_actions,omitempty"`
}

func collectLocalDoctor(ctx context.Context) (report localDoctorReport) {
	defer func() { observeLocalDoctorFailure(ctx, report.failure) }()
	report = localDoctorReport{SetupState: "not_set_up", IdentityState: "missing", CredentialState: "missing", InboxState: "unconfigured", ConfigService: "not_installed", HostRuntime: "not_paired", WorkloadCounts: "unavailable"}
	stateRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	if stateRoot == "" {
		root, err := helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			report.failure = err
			report.SetupState = "error"
			report.RecoveryActions = append(report.RecoveryActions, "set PAPERBOAT_RUNTIME_STATE_ROOT to an absolute private directory")
			return report
		}
		stateRoot = root
	}
	report.StateRoot = stateRoot
	identityPath := filepath.Join(stateRoot, "machine-identity.json")
	registrationPath := filepath.Join(stateRoot, "machine-registration.json")
	if _, err := os.Lstat(registrationPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.RecoveryActions = append(report.RecoveryActions, "run pb setup")
		} else {
			report.failure = err
			report.SetupState, report.IdentityState = "unavailable", "unavailable"
			report.RecoveryActions = append(report.RecoveryActions, "check access to the Paperboat state directory before changing this installation")
		}
		return report
	}
	if info, err := os.Lstat(identityPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		report.failure = err
		report.SetupState, report.IdentityState = "invalid", "invalid"
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			report.SetupState, report.IdentityState = "unavailable", "unavailable"
			report.RecoveryActions = append(report.RecoveryActions, "check access to the Paperboat state directory before changing this installation")
			return report
		}
		report.RecoveryActions = append(report.RecoveryActions, "restore the original machine identity or revoke and set up this installation again")
		return report
	}
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		report.failure = err
		report.SetupState, report.IdentityState = "unavailable", "unavailable"
		if err == identity.ErrInvalidStore || err == identity.ErrKeyConflict {
			report.SetupState, report.IdentityState = "invalid", "invalid"
		}
		report.RecoveryActions = append(report.RecoveryActions, "repair ownership and permissions of the Paperboat state directory")
		return report
	}
	registration, err := store.Registration()
	if err != nil {
		report.failure = err
		report.SetupState, report.IdentityState = "unavailable", "unavailable"
		if err == identity.ErrInvalidStore || err == identity.ErrKeyConflict {
			report.SetupState, report.IdentityState = "invalid", "invalid"
			report.RecoveryActions = append(report.RecoveryActions, "revoke the invalid machine registration and run pb setup")
		} else {
			report.RecoveryActions = append(report.RecoveryActions, "check access to the machine registration before changing this installation")
		}
		return report
	}
	report.SetupState, report.IdentityState = "configured", "valid"
	report.MachineID, report.EnvironmentID = registration.MachineID, registration.EnvironmentID
	report.InstallationGeneration = registration.InstallationGeneration
	report.InboxPath = registration.InboxPath
	if err := inbox.ValidatePath(registration.InboxPath); err != nil {
		report.failure = errors.Join(report.failure, err)
		report.InboxState = "unsafe_or_unavailable"
		report.RecoveryActions = append(report.RecoveryActions, "run pb inbox set <absolute-path> with a private writable directory")
	} else {
		report.InboxState = "ready"
	}
	if control, err := store.MachineControl(time.Now().UTC(), time.Hour); err == nil {
		if control.ExpiresAt.Before(time.Now().UTC()) {
			report.CredentialState = "grace"
		} else {
			report.CredentialState = "valid"
		}
	} else if registration.MachineID != "" {
		report.failure = errors.Join(report.failure, err)
		report.CredentialState = "invalid_or_expired"
		report.RecoveryActions = append(report.RecoveryActions, "run pb pair to renew host authority")
	}
	if runtime.GOOS == "windows" {
		report.ConfigService = windowsConfigServiceStatus()
		if report.ConfigService == "invalid" {
			report.RecoveryActions = append(report.RecoveryActions, "repair the Windows config-sync service")
		}
	} else if home, homeErr := os.UserHomeDir(); homeErr == nil {
		definition := filepath.Join(home, ".config", "systemd", "user", "paperboat-runtime-config.service")
		if runtime.GOOS == "darwin" {
			definition = filepath.Join(home, "Library", "LaunchAgents", service.ConfigLabel+".plist")
		}
		if info, err := os.Lstat(definition); err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			report.ConfigService = localConfigServiceState()
		} else if err == nil {
			report.ConfigService = "invalid"
			report.RecoveryActions = append(report.RecoveryActions, "repair the config-sync service definition")
		} else if !errors.Is(err, os.ErrNotExist) {
			report.failure = errors.Join(report.failure, err)
			report.ConfigService = "unavailable"
			report.RecoveryActions = append(report.RecoveryActions, "check access to the config-sync service definition")
		}
	} else {
		report.failure = errors.Join(report.failure, homeErr)
		report.ConfigService = "unavailable"
	}
	inspectLocalRuntimeHealth(ctx, &report, stateRoot)
	return report
}

func localConfigServiceState() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if runtime.GOOS == "linux" {
		query := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "paperboat-runtime-config.service")
		prepareConfigServiceQuery(query)
		output, err := query.Output()
		state := strings.TrimSpace(string(output))
		if err == nil && state == "active" {
			return "active"
		}
		if state == "inactive" || state == "failed" || state == "activating" || state == "deactivating" {
			return "installed_inactive"
		}
		return "unavailable"
	}
	if runtime.GOOS == "windows" {
		return windowsConfigServiceStatus()
	}
	account, err := user.Current()
	if err != nil {
		return "installed_unknown"
	}
	if err := exec.CommandContext(ctx, "launchctl", "print", "gui/"+account.Uid+"/"+service.ConfigLabel).Run(); err == nil {
		return "active"
	}
	return "installed_inactive"
}

func inspectLocalRuntimeHealth(parent context.Context, report *localDoctorReport, stateRoot string) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	body, err := requestTunnelHostRuntime(ctx, stateRoot, "/healthz", 64<<10)
	if err != nil {
		report.failure = errors.Join(report.failure, err)
		report.HostRuntime = "unavailable · local runtime could not be reached"
		return
	}
	live, err := decodeTunnelDoctorHealth(bytes.NewReader(body), 64<<10)
	if err != nil || !live {
		if err != nil {
			report.failure = errors.Join(report.failure, err)
		} else {
			report.failure = errors.Join(report.failure, errLocalDoctorHealthInvalid)
		}
		report.HostRuntime = "unhealthy · invalid liveness response"
		return
	}
	report.HostRuntime = "ready"
	body, err = requestTunnelHostRuntime(ctx, stateRoot, tunnelHostDiagnosticsPath, tunnelHostDiagnosticsMaxBytes)
	if err != nil {
		report.failure = errors.Join(report.failure, err)
		report.WorkloadCounts = "unavailable · runtime diagnostics could not be read"
		return
	}
	diagnostics, err := decodeTunnelHostDiagnostics(body)
	if err != nil {
		report.failure = errors.Join(report.failure, err)
		report.WorkloadCounts = "unavailable · invalid runtime diagnostics"
		return
	}
	state := diagnostics.Health.Dimensions.Service
	if state.Status != "" && state.Status != "ready" {
		report.HostRuntime = string(state.Status) + " · " + state.Summary
		if state.RepairAction != "" {
			report.RecoveryActions = append(report.RecoveryActions, state.RepairAction)
		}
	}
	if diagnostics.Workloads == nil {
		report.WorkloadCounts = "unavailable · runtime did not provide workload counts"
		return
	}
	report.WorkloadCounts = "available"
	report.TrackedSessions = diagnostics.Workloads.Sessions
	report.ActiveProcesses = diagnostics.Workloads.Processes
	report.ActiveAttachments = diagnostics.Workloads.Attachments
	report.ActiveUploads = diagnostics.Workloads.Uploads
}

func doctorUserMachine(ctx context.Context, client *api.Client, machineID string) (api.UserMachine, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return api.UserMachine{}, err
	}
	for _, machine := range machines {
		if machine.ID == machineID {
			return machine, nil
		}
	}
	return api.UserMachine{}, errors.New("resolved machine is missing from the account")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func refreshSessionIdentification(ctx context.Context, client *api.Client, sessions []machineSession) ([]machineSession, error) {
	targets := make(map[string]environmentTarget)
	for _, entry := range sessions {
		targets[entry.target.id] = entry.target
	}
	var fresh []machineSession
	for _, target := range targets {
		listed, err := listTerminalSessionsForTarget(ctx, client, target)
		if err != nil {
			return nil, err
		}
		for _, entry := range listed {
			fresh = append(fresh, machineSession{target: target, session: entry})
		}
	}
	return fresh, nil
}

func machineSessionIdentificationItem(ctx context.Context, entry machineSession, favorites favoriteSet) selector.Item {
	attached := "no attachments"
	if entry.session.AttachedCount != nil {
		attached = fmt.Sprintf("%d attached", *entry.session.AttachedCount)
	}
	details := preferenceDetails(ctx, "sessions", map[string]string{"machine": entry.target.name, "state": entry.session.State + " · " + attached + " · last active " + relativeTime(entry.session.LastActiveAt), "id": entry.session.ID})
	return selector.Item{ID: entry.target.id + ":" + entry.session.ID, Title: entry.session.Name, Description: sessionIdentificationDetails(entry.session, details), Search: entry.target.name + " " + sessionIdentificationSearch(entry.session) + " favorite starred", Favorite: favorites.IsFavorite("session", machineSessionFavoriteID(entry))}
}
