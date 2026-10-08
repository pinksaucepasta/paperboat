package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

type cyclicStreamError struct{}

func (*cyclicStreamError) Error() string   { return "cyclic" }
func (e *cyclicStreamError) Unwrap() error { return e }

type linkedStreamError struct{ cause error }

func (e linkedStreamError) Error() string { return "linked" }
func (e linkedStreamError) Unwrap() error { return e.cause }

func TestStreamReportsSubstantiveJoinedFailureWithoutExposingErrorText(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()

	const privateText = "private-key-or-output"
	cause := errors.Join(context.Canceled, errors.New(privateText))
	connection := &streamTestConnection{}
	server := &Server{}
	server.wg.Add(1)
	server.stream(context.Background(), &lockedWriter{writer: connection, connection: connection}, connection,
		errorOutputStream{&StreamError{Code: "ssh_target_not_ready", Cause: cause}},
		newTerminalConnectionState(), 1, "ssh.v1")

	if len(faults) != 1 {
		t.Fatalf("observed %d faults, want one", len(faults))
	}
	fault := faults[0]
	if fault.Component != "paperboat-daemon" || fault.Operation != "ssh" || fault.Code != "managed_ssh_failed" || fault.Stage != "delivery" {
		t.Fatalf("fault classification = %#v", fault)
	}
	if strings.Contains(strings.Join(fault.ErrorChain, ","), privateText) || strings.Contains(fault.Cause, privateText) {
		t.Fatalf("fault retained private error text: %#v", fault)
	}
	if len(connection.structured) != 1 || !strings.Contains(string(connection.structured[0].Payload), "ssh_target_not_ready") || strings.Contains(string(connection.structured[0].Payload), privateText) {
		t.Fatalf("protocol frame leaked or omitted the safe failure code: %#v", connection.structured)
	}
}

func TestStreamNormalJoinedTerminationIsNotCaptured(t *testing.T) {
	var faults int
	restore := errorreport.InstallFaultObserver(func(context.Context, errorreport.Fault) { faults++ })
	defer restore()

	connection := &streamTestConnection{}
	server := &Server{}
	server.wg.Add(1)
	server.stream(context.Background(), &lockedWriter{writer: connection, connection: connection}, connection,
		errorOutputStream{&StreamError{Code: "stream_closed", Cause: errors.Join(context.Canceled, io.EOF)}},
		newTerminalConnectionState(), 1, "terminal.v1")
	if faults != 0 {
		t.Fatalf("normal joined termination produced %d faults", faults)
	}
}

func TestExpectedStreamFailureWalkerIsBoundedAndConservative(t *testing.T) {
	if !expectedStreamFailure(errors.Join(context.Canceled, io.EOF)) {
		t.Fatal("all-normal joined termination was not recognized")
	}
	if expectedStreamFailure(errors.Join(context.Canceled, errors.New("failure"))) {
		t.Fatal("mixed cancellation and substantive failure was suppressed")
	}
	if expectedStreamFailure(&cyclicStreamError{}) {
		t.Fatal("cyclic error chain was treated as normal termination")
	}
	var typedNil *cyclicStreamError
	if expectedStreamFailure(typedNil) {
		t.Fatal("typed nil error was treated as normal termination")
	}
	var deep error = io.EOF
	for range 16 {
		deep = linkedStreamError{cause: deep}
	}
	if expectedStreamFailure(deep) {
		t.Fatal("oversized error chain was treated as normal termination")
	}
}
