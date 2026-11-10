//go:build windows

package pty

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
	"time"
)

// Interrupt starts os.Executable() inside the pseudoconsole as `pb daemon __flush-console-input`; here that is the test binary.
func TestMain(m *testing.M) {
	if slices.Equal(os.Args[1:], FlushConsoleInputArgs) {
		if err := FlushConsoleInput(); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Process over a plain pipe: nothing reads r, so a large write blocks like a paste into a program that never reads.
func pipeProcess(t *testing.T) (*Process, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	return &Process{input: w}, r
}

func startBlockedWrite(p *Process) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := p.WriteContext(context.Background(), make([]byte, 4<<20))
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	return done
}

// The journal finalizes a cut write only on ErrInterrupted (session/input.go); ErrDeadlineExceeded would be resent after Ctrl-C.
func TestInterruptEndsABlockedWriteWithErrInterrupted(t *testing.T) {
	p, r := pipeProcess(t)
	blocked := startBlockedWrite(p)
	result := make(chan error, 1)
	go func() {
		handled, err := p.Interrupt(0x03, nil)
		if !handled && err == nil {
			err = errors.New("not handled")
		}
		result <- err
	}()
	select {
	case err := <-blocked:
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("the cut write ended with %v, want ErrInterrupted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked write was not cancelled")
	}
	go io.Copy(io.Discard, r) // room for the key
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Interrupt did not return")
	}
}

// A key that cannot be written must not be reported as delivered: the caller would drop the input queued before it.
func TestInterruptThatCannotTakeTheInputLockIsNotHandled(t *testing.T) {
	p, _ := pipeProcess(t)
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	accepted := false
	handled, err := p.Interrupt(0x03, func() { accepted = true })
	if handled || err == nil {
		t.Fatalf("Interrupt handled=%v err=%v, want not handled with an error", handled, err)
	}
	if !accepted {
		t.Fatal("accepted was not called before the lock wait")
	}
}

// A closed input handle may be reused by the OS: cancelling it would cancel someone else's I/O.
func TestCancelInputLeavesAClosedProcessAlone(t *testing.T) {
	p, _ := pipeProcess(t)
	blocked := startBlockedWrite(p)
	p.mu.Lock()
	p.inputClosed = true
	p.mu.Unlock()
	p.cancelInput()
	select {
	case err := <-blocked:
		t.Fatalf("a closed process had its pending write cancelled (%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	p.mu.Lock()
	p.inputClosed = false
	p.mu.Unlock()
	p.cancelInput()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelInput did not cancel the pending write of an open process")
	}
}
