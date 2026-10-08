package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type diagnosticProcess struct {
	readErr  error
	waitErr  error
	closeErr error
}

func (p *diagnosticProcess) Read([]byte) (int, error)     { return 0, p.readErr }
func (*diagnosticProcess) Write(data []byte) (int, error) { return len(data), nil }
func (*diagnosticProcess) Resize(pty.Dimensions) error    { return nil }
func (*diagnosticProcess) Signal(pty.Signal) error        { return nil }
func (p *diagnosticProcess) Wait(context.Context) (pty.ExitResult, error) {
	return pty.ExitResult{Code: 9, ExitedAt: time.Now().UTC()}, p.waitErr
}
func (p *diagnosticProcess) Terminate(context.Context, time.Duration) (pty.ExitResult, error) {
	return pty.ExitResult{Code: 0, ExitedAt: time.Now().UTC()}, nil
}
func (p *diagnosticProcess) CloseIO() error { return p.closeErr }

type cyclicDiagnosticError struct{ next error }

func (e *cyclicDiagnosticError) Error() string { return "cycle" }
func (e *cyclicDiagnosticError) Unwrap() error { return e.next }

func TestNormalSessionTerminationRequiresOnlyBoundedNormalLeaves(t *testing.T) {
	if !normalSessionTermination(errors.Join(io.EOF, context.Canceled)) {
		t.Fatal("normal joined termination was not recognized")
	}
	if normalSessionTermination(errors.Join(context.Canceled, errors.New("substantive failure"))) {
		t.Fatal("joined substantive failure was suppressed")
	}
	cycle := &cyclicDiagnosticError{}
	cycle.next = cycle
	if normalSessionTermination(cycle) {
		t.Fatal("cyclic error was treated as normal termination")
	}
	var typedNil *cyclicDiagnosticError
	if normalSessionTermination(typedNil) {
		t.Fatal("typed nil error was treated as normal termination")
	}
	deep := error(io.EOF)
	for range 16 {
		deep = &cyclicDiagnosticError{next: deep}
	}
	if normalSessionTermination(deep) {
		t.Fatal("over-budget error chain was treated as normal termination")
	}
}

func TestSessionFailureKeepsCauseAndSafeText(t *testing.T) {
	privateText := "private-command-fragment"
	cause := errors.New(privateText)
	failure := classifySessionFailure("command", cause)
	if !errors.Is(failure, cause) {
		t.Fatal("classified failure did not preserve its cause")
	}
	if failure.Error() == privateText || strings.Contains(failure.Error(), privateText) {
		t.Fatalf("classified failure exposed its cause: %q", failure.Error())
	}
	var diagnostic interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if !errors.As(failure, &diagnostic) || diagnostic.DiagnosticStage() != "command" || diagnostic.DiagnosticCode() != "terminal_session_failed" {
		t.Fatalf("classified diagnostics=%#v", diagnostic)
	}
}

func TestCaptureObservesUnexpectedReadFailureWithoutPrivateText(t *testing.T) {
	observed := make(chan errorreport.Fault, 4)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		observed <- fault
	})
	defer restore()

	privateText := "terminal-payload-must-not-appear"
	process := &diagnosticProcess{readErr: errors.Join(io.EOF, errors.New(privateText))}
	manager, err := NewManager(ManagerConfig{Launch: func(pty.Command) (PTYProcess, error) { return process, nil }})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	created, err := manager.Create(supportref.WithContext(context.Background(), reference), CreateRequest{Name: "diagnostic", Command: pty.Command{Path: "/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Snapshot(created.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case fault := <-observed:
		if fault.Component != "paperboat-daemon" || fault.Operation != "sessions" || fault.Stage != "delivery" || fault.Code != "terminal_session_failed" || fault.SupportReference != reference {
			t.Fatalf("fault metadata=%+v", fault)
		}
		if fault.Cause == privateText || fault.ErrorType == privateText || fault.SupportReference == "" {
			t.Fatalf("unsafe fault metadata=%+v", fault)
		}
	case <-time.After(time.Second):
		t.Fatal("unexpected PTY read failure was not observed")
	}
}

func TestCaptureDoesNotReportNonzeroUserExit(t *testing.T) {
	observed := make(chan errorreport.Fault, 1)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		observed <- fault
	})
	defer restore()

	process := &diagnosticProcess{readErr: io.EOF}
	manager, err := NewManager(ManagerConfig{Launch: func(pty.Command) (PTYProcess, error) { return process, nil }})
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Name: "expected-exit", Command: pty.Command{Path: "/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := manager.Snapshot(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == Exited {
			if snapshot.Exit == nil || snapshot.Exit.Code != 9 {
				t.Fatalf("user exit result=%#v", snapshot.Exit)
			}
			select {
			case fault := <-observed:
				t.Fatalf("nonzero user exit emitted exception: %+v", fault)
			default:
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("session did not reach Exited")
}
