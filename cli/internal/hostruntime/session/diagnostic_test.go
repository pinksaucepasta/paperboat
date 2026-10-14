package session

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"io"
	"math"
	"strings"
	"sync"
	"syscall"
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

// Delivery failure must stop the PTY rather than silently consume later output.
func TestCaptureStopsUndeliverableOutputAndRecovers(t *testing.T) {
	for _, exhausted := range []bool{true, false} {
		name := "fanout order"
		if exhausted {
			name = "history sequence"
		}
		t.Run(name, func(t *testing.T) {
			observed := make(chan errorreport.Fault, 4)
			restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed <- fault })
			defer restore()
			process := &deliveryFailureProcess{ready: make(chan struct{}), stopped: make(chan struct{})}
			launches := 0
			manager, err := NewManager(ManagerConfig{Launch: func(pty.Command) (PTYProcess, error) {
				launches++
				if launches == 1 {
					return process, nil
				}
				return &diagnosticProcess{readErr: io.EOF}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			reference := supportref.New()
			created, err := manager.Create(supportref.WithContext(t.Context(), reference), CreateRequest{Name: "delivery", Command: pty.Command{Path: "/bin/sh"}})
			if err != nil {
				t.Fatal(err)
			}
			session, err := manager.get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			session.opMu.Lock()
			if exhausted {
				session.history, err = history.Restore(manager.config.HistoryBytes, math.MaxUint64, math.MaxUint64, nil)
			} else {
				err = session.fanout.Attach("private-attachment", 1024)
				if err == nil {
					_, err = session.fanout.EnqueueOwned("private-attachment", history.Event{Channel: 1, StartSequence: 10, EndSequence: 11, Data: []byte("x")})
				}
			}
			session.opMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			close(process.ready)
			defer process.Terminate(context.Background(), 0)
			select {
			case <-process.stopped:
			case <-time.After(time.Second):
				t.Fatal("undeliverable PTY was not terminated")
			}
			select {
			case fault := <-observed:
				if fault.Stage != "delivery" || fault.SupportReference != reference || fault.Code != "terminal_session_failed" {
					t.Fatalf("delivery fault metadata=%+v", fault)
				}
				if strings.Contains(fault.ErrorType, "private-") || strings.Contains(strings.Join(fault.ErrorChain, ":"), "private-") {
					t.Fatal("terminal or attachment payload exposed")
				}
			case <-time.After(time.Second):
				t.Fatal("delivery failure was not observed")
			}
			deadline := time.Now().Add(time.Second)
			for {
				snapshot, err := manager.Snapshot(created.ID)
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.State == Exited {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("failed session remained running")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case fault := <-observed:
				t.Fatalf("duplicate failure=%+v", fault)
			default:
			}
			if _, err := manager.Create(t.Context(), CreateRequest{Name: "recovered", Command: pty.Command{Path: "/bin/sh"}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type deliveryFailureProcess struct {
	diagnosticProcess
	ready, stopped chan struct{}
	once           sync.Once
	read           bool
	terminateMu    sync.Mutex
	terminateErr   error
}

func (p *deliveryFailureProcess) Read(buffer []byte) (int, error) {
	<-p.ready
	if !p.read {
		p.read = true
		return copy(buffer, "private-terminal-output"), nil
	}
	<-p.stopped
	return 0, io.EOF
}
func (p *deliveryFailureProcess) Wait(context.Context) (pty.ExitResult, error) {
	<-p.stopped
	return pty.ExitResult{Code: 0, ExitedAt: time.Now()}, nil
}
func (p *deliveryFailureProcess) Terminate(context.Context, time.Duration) (pty.ExitResult, error) {
	p.terminateMu.Lock()
	defer p.terminateMu.Unlock()
	if p.terminateErr != nil {
		err := p.terminateErr
		p.terminateErr = nil
		return pty.ExitResult{}, err
	}
	p.once.Do(func() { close(p.stopped) })
	return pty.ExitResult{Code: 0, ExitedAt: time.Now()}, nil
}

func TestCaptureRetainsProcessWhenDeliveryTerminationFails(t *testing.T) {
	observed := make(chan errorreport.Fault, 4)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed <- fault })
	defer restore()
	process := &deliveryFailureProcess{ready: make(chan struct{}), stopped: make(chan struct{}), terminateErr: syscall.EIO}
	defer process.Terminate(context.Background(), 0)
	manager, err := NewManager(ManagerConfig{Launch: func(pty.Command) (PTYProcess, error) { return process, nil }})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	created, err := manager.Create(supportref.WithContext(t.Context(), reference), CreateRequest{Name: "termination", Command: pty.Command{Path: "/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := manager.get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	owned.opMu.Lock()
	owned.history, err = history.Restore(manager.config.HistoryBytes, math.MaxUint64, math.MaxUint64, nil)
	owned.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	close(process.ready)
	for range 2 {
		select {
		case fault := <-observed:
			if fault.SupportReference != reference {
				t.Fatal("cleanup reference lost")
			}
		case <-time.After(time.Second):
			t.Fatal("delivery/termination cause not observed")
		}
	}
	snapshot, err := manager.Snapshot(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != Closing || snapshot.Exit != nil {
		t.Fatal("unconfirmed termination falsely published an exit")
	}
	select {
	case <-process.stopped:
		t.Fatal("fixture termination unexpectedly completed")
	default:
	}
	closed, err := manager.Close(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != Closed || closed.Exit == nil || closed.Exit.ExitedAt.IsZero() {
		t.Fatal("later close did not reap retained process")
	}
	select {
	case <-process.stopped:
	default:
		t.Fatal("later close lost live process ownership")
	}
}
