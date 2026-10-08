package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type execSecretError struct{}

func (execSecretError) Error() string { panic("private error text evaluated") }

func TestExecFailurePreservesCauseAndMixedOutcomeWithoutPrivateText(t *testing.T) {
	for _, err := range []error{execSecretError{}, errors.Join(&tunnel.RemoteExecError{Code: "exec_canceled"}, syscall.EIO), &tunnel.RemoteExecError{Code: "PRIVATE_PAYLOAD"}} {
		var out, stderr bytes.Buffer
		result := finishExecResult(&out, &stderr, "operation", true, false, 0, err)
		if !errors.Is(result, err) {
			t.Fatal("original cause lost")
		}
		failure := classifyCommandFailure(result)
		if failure.kind != commandUnexpected || !failure.presented {
			t.Fatalf("classification=%+v", failure)
		}
		var exit interface{ ExitCode() int }
		if !errors.As(result, &exit) || exit.ExitCode() != 255 || !strings.Contains(out.String(), "transport_lost") {
			t.Fatal("reserved transport result missing")
		}
		if strings.Contains(out.String(), "PRIVATE") || stderr.Len() != 0 {
			t.Fatal("private text exported")
		}
	}
}
func TestExecBorrowedPipeCancellationJoinsAndPreservesInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { var value [8]byte; _, err := readExecInput(ctx, reader, value[:]); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("borrowed stdin worker remained alive")
	}
	if _, err := writer.Write([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	var value [8]byte
	n, err := readExecInput(context.Background(), reader, value[:])
	if err != nil || string(value[:n]) != "fresh" {
		t.Fatalf("fresh=%q err=%v", value[:n], err)
	}
}

type execReadFault struct{}

func (execReadFault) Read([]byte) (int, error) { return 0, errors.Join(io.EOF, syscall.EIO) }
func TestExecInputMixedEOFDoesNotAssertSuccessfulHalfClose(t *testing.T) {
	connection := &execInputTestConn{}
	err := forwardExecInput(context.Background(), execReadFault{}, newExecConnectionRef(connection))
	if !errors.Is(err, syscall.EIO) || connection.closeWrite {
		t.Fatalf("input cause/half-close=%v/%v", err, connection.closeWrite)
	}
}

func TestExecCancelAttachmentDeadlineJoinsNetworkReaderAndDrainer(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	connection, err := tunnel.NewLocalExecPeerConn(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cancelExecAttachment(ctx, connection).err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("actual blocked-write deadline lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("attachment workers did not join")
	}
	select {
	case _, ok := <-connection.Events():
		if ok {
			t.Fatal("output drain remains open")
		}
	default:
		t.Fatal("network reader remains alive")
	}
}

func TestExecDeadlineAndCancellationPreserveTimeoutWithoutMaskingIO(t *testing.T) {
	for _, test := range []struct {
		cause error
		code  int
		kind  commandFailureKind
	}{
		{errors.Join(context.DeadlineExceeded, &tunnel.RemoteExecError{Code: "exec_canceled"}), 124, commandDeadline},
		{errors.Join(context.DeadlineExceeded, &tunnel.RemoteExecError{Code: "exec_canceled"}, syscall.EIO), 255, commandUnexpected},
	} {
		var out, stderr bytes.Buffer
		err := finishExecResult(&out, &stderr, "operation", true, true, 0, test.cause)
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || exit.ExitCode() != test.code || classifyCommandFailure(err).kind != test.kind || !errors.Is(err, test.cause) {
			t.Fatalf("exit/classification/cause mismatch: %v", err)
		}
		if out.Len() != 0 || stderr.Len() != 0 {
			t.Fatal("terminal result was duplicated")
		}
	}
}

type execCompletedRemote struct {
	execInputTestConn
	events chan tunnel.ExecEvent
}

func (r *execCompletedRemote) Events() <-chan tunnel.ExecEvent { return r.events }
func (r *execCompletedRemote) Wait() (int, error)              { return 37, nil }

func TestExecCancelAttachmentRetainsAlreadyCompletedNativeResult(t *testing.T) {
	client, server := net.Pipe()
	remote := &execCompletedRemote{events: make(chan tunnel.ExecEvent, 1)}
	remote.events <- tunnel.ExecEvent{OperationID: "operation", State: "exited", Result: &tunnel.ExecResult{Code: 37}}
	served := make(chan error, 1)
	go func() { served <- tunnel.ServeLocalPeerConn(context.Background(), server, remote) }()
	connection, err := tunnel.NewLocalExecPeerConn(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outcome := cancelExecAttachment(ctx, connection)
	if outcome.err != nil || outcome.code != 37 || outcome.terminal == nil || outcome.terminal.Result == nil || outcome.terminal.Result.Code != 37 {
		t.Fatalf("actual attachment result=%+v", outcome)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server workers did not join")
	}
	var out, stderr bytes.Buffer
	result := finishExecResult(&out, &stderr, "operation", true, true, outcome.code, outcome.err)
	var exit interface{ ExitCode() int }
	if !errors.As(result, &exit) || exit.ExitCode() != 37 || out.Len() != 0 {
		t.Fatal("completed native exit changed or output duplicated")
	}
}

func TestExecPresentedCycleFirstJoinSelectsOwnerBoundedly(t *testing.T) {
	cause := errors.Join(&cyclicCommandError{}, execCommandFailure{cause: &tunnel.RemoteExecError{Code: "exec_canceled"}, code: 130, presented: true})
	done := make(chan commandFailure, 1)
	go func() { done <- classifyCommandFailure(cause) }()
	select {
	case failure := <-done:
		if failure.kind != commandUnexpected || !failure.presented || failure.execCount != 1 || failure.execCode != 130 {
			t.Fatal("bounded mixed presented owner was lost")
		}
		if presentedCommandExitCode(failure) != 1 {
			t.Fatal("independent cycle incorrectly retained cancellation status")
		}
	case <-time.After(time.Second):
		t.Fatal("presented owner selection traversed cycle without bound")
	}
	multiple := classifyCommandFailure(errors.Join(execCommandFailure{cause: context.Canceled, code: 130, presented: true}, execCommandFailure{cause: context.DeadlineExceeded, code: 124, presented: true}))
	if presentedCommandExitCode(multiple) != 1 {
		t.Fatal("multiple private exec outcomes did not fail closed")
	}
}

type execAbortTrackingConn struct {
	execInputTestConn
	aborted bool
}

func (c *execAbortTrackingConn) Abort() error { c.aborted = true; return nil }
func TestExecInputWriteFailureAbortsItsOwnedAttachment(t *testing.T) {
	connection := &execAbortTrackingConn{execInputTestConn: execInputTestConn{writeErr: syscall.EIO}}
	refs := newExecConnectionRef(connection)
	err := forwardExecInput(context.Background(), strings.NewReader("input"), refs)
	if !errors.Is(err, syscall.EIO) || !connection.aborted || refs.Current() != nil {
		t.Fatal("failed writer lost cause or left detached reader alive")
	}
	if execInputClosedOnly(errors.Join(syscall.EPIPE, syscall.EIO)) || !execInputClosedOnly(syscall.EPIPE) {
		t.Fatal("independent I/O failure was treated as a closed input")
	}
}
