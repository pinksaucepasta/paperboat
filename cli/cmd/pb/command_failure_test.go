package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/api"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

type commandPrivateError struct{ calls *int }

func (e *commandPrivateError) Error() string { *e.calls++; return "PRIVATE_COMMAND_PAYLOAD" }

type cyclicCommandError struct{}

func (*cyclicCommandError) Error() string   { panic("must not format cyclic error") }
func (e *cyclicCommandError) Unwrap() error { return e }

func TestCommandMixedFailuresNeverHideIndependentOperationalCause(t *testing.T) {
	calls := 0
	private := &commandPrivateError{calls: &calls}
	cases := []error{
		errors.Join(context.Canceled, syscall.EIO),
		errors.Join(&net.DNSError{Err: "no such host", Name: "PRIVATE_DNS_NAME"}, syscall.EIO),
		errors.Join(api.ErrUnauthenticated, syscall.EIO),
		errors.Join(&api.APIError{Status: 403, Code: "team_subscription_required"}, private),
		errors.Join(managedssh.NativeExitError{Code: 7, Err: private}, syscall.EIO),
		errors.Join(invocationError(private), syscall.EIO),
		errors.Join(&TunnelCreateExistingError{Cause: private}, syscall.EIO),
		errors.Join(context.Canceled, &cyclicCommandError{}),
	}
	for _, err := range cases {
		classified := classifyCommandFailure(err)
		if classified.kind != commandUnexpected || !unexpectedCLIError(err) || nativeExitFailure(err) {
			t.Fatalf("mixed failure suppressed: kind=%d", classified.kind)
		}
		value := classifyCLIJSONError(err)
		if value.StateChanged != "unknown" || value.Category == "canceled" || value.Category == "usage" || strings.Contains(value.Message, "No resource was created") || strings.Contains(value.Message, "PRIVATE") {
			t.Fatalf("mixed public result=%#v", value)
		}
	}
	if calls != 0 {
		t.Fatalf("formatted private errors %d times", calls)
	}
	pure := fmt.Errorf("private wrapper: %w", context.Canceled)
	if classifyCommandFailure(pure).kind != commandCanceled {
		t.Fatal("pure cancellation rejected")
	}
	var typedNil *commandPrivateError
	if classifyCommandFailure(errors.Join(context.Canceled, typedNil)).kind != commandUnexpected {
		t.Fatal("typed nil falsely proved cancellation")
	}
	deadline := errors.Join(context.Canceled, context.DeadlineExceeded)
	if classifyCommandFailure(deadline).kind != commandDeadline {
		t.Fatal("cancellation masked deadline")
	}
}

func TestCommandTypedOutputsDoNotFormatPrivateCause(t *testing.T) {
	calls := 0
	private := &commandPrivateError{calls: &calls}
	for _, err := range []error{
		&TunnelCreateExistingError{Name: "PRIVATE_NAME", TunnelID: "PRIVATE_ID", RecoveryCommand: "PRIVATE_COMMAND", Cause: private},
		&TunnelCreateChangedError{Stage: "PRIVATE_STAGE", Cause: private, RecoveryCommand: "PRIVATE_COMMAND"},
		&TunnelOperationWaitTimeoutError{OperationID: "PRIVATE_ID"},
		invocationError(private),
	} {
		wrapped := fmt.Errorf("PRIVATE_WRAPPER: %w", err)
		calls = 0 // Error construction belongs to the producer; the export boundary must not format it.
		result := classifyCLIJSONError(wrapped)
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatal("private cause formatted")
		}
		if strings.Contains(string(encoded), "PRIVATE") {
			t.Fatalf("private public result=%s", encoded)
		}
	}
	if calls != 0 {
		t.Fatal("private cause formatted")
	}
}

func TestCommandLocalConstraintPreservesCauseWithoutProviderDetails(t *testing.T) {
	const constraint = "Flags --team and --private are mutually exclusive. Choose one audience."
	calls := 0
	cause := &commandPrivateError{calls: &calls}
	err := usageError{err: cause, publicMessage: constraint}
	if !errors.Is(err, cause) || !errors.Is(err, errUsage) || classifyCommandFailure(err).kind != commandUsage {
		t.Fatal("local constraint lost its usage classification or cause")
	}
	public := classifyCLIJSONError(err)
	if public.Code != "invalid_invocation" || public.Message != constraint || userFacingError(err) != constraint || calls != 0 {
		t.Fatalf("local constraint presentation=%#v private formats=%d", public, calls)
	}
	generic := classifyCLIJSONError(invocationError(cause))
	if strings.Contains(generic.Message, "PRIVATE") || calls != 0 {
		t.Fatal("untrusted invocation detail was exposed")
	}
	mixed := classifyCLIJSONError(errors.Join(err, syscall.EIO))
	if mixed.Category == "usage" || mixed.Message == constraint || calls != 0 {
		t.Fatal("local constraint hid an independent operational failure")
	}
}

func TestCommandUnknownFlagKeepsOneSafeJSONAndUsageExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"--json", "--PRIVATE_UNKNOWN_FLAG=PRIVATE_VALUE"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit=%d", code)
	}
	var result cliJSONEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || result.Error.Code != "invalid_invocation" || stderr.Len() != 0 || strings.Contains(result.Error.Message, "PRIVATE") {
		t.Fatalf("result=%#v stderr=%q", result, stderr.String())
	}
}

func TestDoctorNativeTimeoutPreservesJoinedSystemCauseAndReference(t *testing.T) {
	restore := errorreport.Install(nil)
	defer restore()
	reference := supportref.New()
	ctx, cancel := context.WithTimeout(supportref.WithContext(t.Context(), reference), 10*time.Millisecond)
	defer cancel()
	var fault errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { fault = f })
	defer restoreObserver()
	probe := doctorNativeReachabilityProbe(ctx, time.Second, func(loadCtx context.Context) (tunnel.NativeProbe, error) {
		<-loadCtx.Done()
		return tunnel.NativeProbe{}, syscall.EIO
	})[0]
	check := probe.Run(ctx)
	if fault.SupportReference != reference || fault.Cause != "deadline_exceeded" || !strings.Contains(strings.Join(fault.ErrorChain, ":"), "Errno") || !strings.Contains(check.Summary, "timed out") {
		t.Fatalf("joined timeout fault=%#v check=%#v", fault, check)
	}
}

type failingDoctorClose struct{ cause error }

func (c failingDoctorClose) Close() error { return c.cause }
func TestDoctorUDPFailedCleanupIsObservedThenHealthyProbeRecovers(t *testing.T) {
	restore := errorreport.Install(nil)
	defer restore()
	reference := supportref.New()
	ctx := supportref.WithContext(t.Context(), reference)
	var fault errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { fault = f })
	defer restoreObserver()
	check := finishDoctorUDP(ctx, failingDoctorClose{syscall.EIO}, "udp_ipv4", "IPv4")
	if check.Status != doctorpkg.StatusWarning || check.Recovery == "" || fault.SupportReference != reference || fault.Errno != int(syscall.EIO) {
		t.Fatalf("cleanup diagnostic check=%#v fault=%#v", check, fault)
	}
	healthy := probeDoctorUDP(ctx, "udp4", "127.0.0.1:0", "udp_ipv4", "IPv4")
	if healthy.Status != doctorpkg.StatusPass {
		t.Fatalf("healthy retry=%#v", healthy)
	}
	if !errors.Is(failingDoctorClose{syscall.EIO}.Close(), syscall.EIO) {
		t.Fatal("original cleanup cause lost")
	}
}

func TestCommandNetworkWrappersPreserveCancellationAndMixedCause(t *testing.T) {
	for _, test := range []struct {
		cause error
		kind  commandFailureKind
	}{
		{&url.Error{Op: "PRIVATE_OPERATION", URL: "https://PRIVATE", Err: context.Canceled}, commandCanceled},
		{&url.Error{Err: context.DeadlineExceeded}, commandDeadline},
		{&net.OpError{Op: "PRIVATE", Err: errors.Join(context.Canceled, syscall.EIO)}, commandUnexpected},
		{&url.Error{Err: syscall.ECONNREFUSED}, commandOperational},
		{&url.Error{Err: execSecretError{}}, commandOperational},
		{&url.Error{Err: errors.Join(context.Canceled, execSecretError{})}, commandUnexpected},
	} {
		failure := classifyCommandFailure(test.cause)
		if failure.kind != test.kind {
			t.Fatalf("network classification=%d want=%d", failure.kind, test.kind)
		}
		if strings.Contains(userFacingError(test.cause), "PRIVATE") {
			t.Fatal("private network metadata exported")
		}
	}
}
