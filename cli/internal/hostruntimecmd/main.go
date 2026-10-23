package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// MaintenanceBusyExitCode preserves a pre-stop refusal across Unix elevation.
const MaintenanceBusyExitCode = 75
const UpdateVerificationExitCode = 76
const LegacyCarrierRunningExitCode = 77

// InsufficientSpaceExitCode preserves a staging refusal for lack of disk space across Unix elevation.
const InsufficientSpaceExitCode = 78

// UpdateNotReadyExitCode preserves "the new version did not start, the previous one was restored"
// across Unix elevation.
const UpdateNotReadyExitCode = 79

const usage = `pb daemon internal host runtime.

Usage:
  pb daemon __runtime-hostd
  pb daemon __runtime-worker
  pb daemon __runtime-session-owner
  pb daemon __runtime-updated
  pb daemon __runtime-activate
  pb daemon __runtime-local-daemon

This entry point is managed by Paperboat services and is not a user command.`

func run(args []string, stdout, stderr io.Writer) int {
	return execute(context.Background(), args, os.Stdin, stdout, stderr)
}

// Execute runs a validated host-runtime mode from the endpoint daemon or short-lived service management command.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if ctx == nil || stdin == nil || stdout == nil || stderr == nil {
		return 2
	}
	return execute(ctx, args, stdin, stdout, stderr)
}

func execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if _, err := fmt.Fprintln(stdout, usage); err != nil {
			return finishRuntimeCommand(ctx, "help", stderr, runtimeFailure("host runtime help could not be written", err), 1)
		}
		return 0
	}

	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		if _, err := fmt.Fprintf(stdout, "pb %s (%s)\n", buildinfo.Version, buildinfo.Commit); err != nil {
			return finishRuntimeCommand(ctx, "version", stderr, runtimeFailure("host runtime version could not be written", err), 1)
		}
		return 0
	}
	if args[0] == "bootstrap" {
		if err := runBootstrap(ctx, args[1:], stdin, stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "service" {
		if err := runServiceCommand(ctx, args[1:], stdin, stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, serviceFailureExitCode(err))
		}
		return 0
	}
	if args[0] == "purge" {
		if err := runPurgeCommand(ctx, args[1:], stdin, stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "doctor" {
		if err := runDoctor(ctx, args[1:], stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "run" {
		if len(args) != 1 {
			return finishRuntimeCommand(ctx, "daemon", stderr, runtimeFailure("run does not accept arguments"), 2)
		}
		if err := runProduction(ctx, stdout); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "hostd" {
		if err := runHostd(ctx, args[1:], stdout); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "session-owner" {
		if err := runSessionOwner(ctx, args[1:], stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "carrier" {
		if err := runCarrier(ctx, args[1:], stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "worker" {
		if err := runWorker(ctx, args[1:], stdin, stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "updated" {
		if err := runUpdated(ctx, args[1:], stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "activate" {
		if err := runActivator(ctx, args[1:], stdout, stderr); err != nil {
			return finishRuntimeCommand(ctx, args[0], stderr, err, 1)
		}
		return 0
	}
	if args[0] == "local-daemon-service" {
		return executeLocalDaemonService(ctx, args[1:], stdout, stderr)
	}

	return finishRuntimeCommand(ctx, "command", stderr, runtimeFailure("unknown command"), 2)
}

func writeError(w io.Writer, err error) {
	if err == nil {
		return
	}
	_, _ = fmt.Fprintln(w, "pb: "+runtimeFailureMessage(err))
}

func runtimeFailureMessage(err error) string {
	if runtimeCausesOnly(err, hostinstall.ErrLegacyCarrierRunning) {
		return hostinstall.ErrLegacyCarrierRunning.Error()
	}
	if failure, ok := err.(*CommandFailure); ok && failure != nil {
		return failure.message
	}
	var remote *elevation.RemoteError
	if errors.As(err, &remote) && remote != nil {
		if message := elevation.RuntimeInstallFailureMessage(remote.Code); message != "" {
			return message + " Run pb doctor, then retry."
		}
	}
	switch {
	case errors.Is(err, bootstrap.ErrEnrollmentTokenExpired):
		return "The enrollment token expired. Create a new enrollment with Add machine and retry."
	case errors.Is(err, bootstrap.ErrEnrollmentTokenUsed):
		return "The enrollment token was already used. Rerun the original pairing command on its machine to resume it, or create a new enrollment with Add machine."
	case errors.Is(err, bootstrap.ErrEnrollmentTokenInvalid):
		return "The enrollment token was revoked or invalid. Create a new enrollment with Add machine and retry."
	case errors.Is(err, bootstrap.ErrPairingPlatformUnsupported):
		return "This enrollment command does not support this operating system. Create a new enrollment with Add machine and choose its command for this operating system."
	case errors.Is(err, bootstrap.ErrPairingExpired):
		return "The machine pairing expired. Create a new enrollment with Add machine and retry."
	case errors.Is(err, bootstrap.ErrInstallationUnavailable):
		return "This enrollment was already used or is no longer available. Create a new enrollment with Add machine and retry."
	case errors.Is(err, bootstrap.ErrResumeBinding):
		return "An unfinished earlier enrollment on this machine used a different enrollment. Run `pb reset` with this enrollment token file to discard it, then pair again."
	case errors.Is(err, bootstrap.ErrPairingDenied):
		return "The machine pairing was denied. Create a new enrollment with Add machine if this machine should be added."
	}
	var request bootstrap.RequestError
	if errors.As(err, &request) {
		return fmt.Sprintf("Machine pairing request failed: %s. Check the server connection and retry pairing.", request.Error())
	}
	var stage *installationStageError
	if errors.As(err, &stage) && stage != nil {
		return stage.Error()
	}
	if message, ok := hostinstall.FailureMessage(err); ok {
		return "Paperboat installation failed: " + message + ". Run pb doctor, then retry."
	}
	return "Paperboat host runtime command failed; run pb doctor, then retry."
}

// Integer-returning commands own their original failure before returning a
// native status to the outer CLI. The outer owner must not invent an exception.
func finishRuntimeCommand(ctx context.Context, operation string, output io.Writer, err error, code int) int {
	if err == nil {
		return code
	}
	serviceOperation := operation
	switch operation {
	case "bootstrap":
		operation = "pair"
	case "hostd", "worker", "session-owner", "run", "local-daemon-service":
		operation = "daemon"
	case "updated", "activate":
		operation = "update"
	case "purge":
		operation = "uninstall"
	default:
		operation = errorreport.Operation(operation)
	}
	outputFailed := false
	message := "pb: " + runtimeFailureMessage(err)
	if longRunningRuntimeService(serviceOperation) {
		// These run under the service manager with nobody to ask. The journal is
		// the only place their real cause can be found, and support references
		// alone do not carry it.
		message += "\npb: cause: " + FailureCauseText(err)
	}
	if _, writeErr := fmt.Fprintln(output, message); writeErr != nil {
		outputFailed = true
		if code == LegacyCarrierRunningExitCode {
			code = 1
		}
		err = runtimeFailure("host runtime command failed and its result could not be written", err, writeErr)
	}
	unowned := unreportedRuntimeFailure(err)
	fault := errorreport.ProjectFault(ctx, "paperboat-daemon", operation, "command", "command_failed", err)
	if unowned == nil {
		return code
	}
	if (code == LegacyCarrierRunningExitCode && !outputFailed && runtimeCausesOnly(err, hostinstall.ErrLegacyCarrierRunning)) || (code == 2 && !outputFailed) || fault.Outcome == "canceled" || fault.Outcome == "rejected" || operation == "doctor" {
		observeRuntimeFailure(ctx, operation, err)
	} else {
		errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", operation, "command", "unexpected_cli_failure", unowned)
	}
	return code
}

// Only a direct refusal proves that no independent failure changed installation
// state. Joined or wrapped failures retain the generic uncertain exit status.
func serviceFailureExitCode(err error) int {
	if runtimeCausesOnly(err, hostinstall.ErrLegacyCarrierRunning) {
		return LegacyCarrierRunningExitCode
	}
	if _, busyTimeout := err.(*hostinstall.MaintenanceBusyTimeoutError); err == hostdproto.ErrMaintenanceBusy || busyTimeout {
		return MaintenanceBusyExitCode
	}
	if err == hostinstall.ErrUpdateVerificationFailed {
		return UpdateVerificationExitCode
	}
	if errors.Is(err, hostinstall.ErrInsufficientSpace) {
		return InsufficientSpaceExitCode
	}
	if errors.Is(err, hostinstall.ErrUpdateNotReady) {
		return UpdateNotReadyExitCode
	}
	return 1
}

// longRunningRuntimeService reports the commands that systemd or launchd starts.
func longRunningRuntimeService(operation string) bool {
	switch operation {
	case "hostd", "worker", "session-owner", "carrier", "updated":
		return true
	}
	return false
}

// FailureCauseText lists the distinct leaf errors under err, bounded in depth
// and length. A CommandFailure's own message is public prose; the leaves are
// the diagnostic causes it retains.
func FailureCauseText(err error) string {
	var leaves []string
	var walk func(error, int)
	walk = func(current error, depth int) {
		if current == nil || len(leaves) >= 8 {
			return
		}
		var children []error
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children = joined.Unwrap()
		}
		if len(children) == 0 || depth >= 6 {
			text := current.Error()
			if len(text) > 300 {
				text = text[:300] + "..."
			}
			if !slices.Contains(leaves, text) {
				leaves = append(leaves, text)
			}
			return
		}
		for _, child := range children {
			walk(child, depth+1)
		}
	}
	walk(err, 0)
	return strings.Join(leaves, "; ")
}
