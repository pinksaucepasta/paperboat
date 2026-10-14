package selector

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

type selectorFailure struct{ cause error }

func (selectorFailure) Error() string   { return "interactive selection failed" }
func (f selectorFailure) Unwrap() error { return f.cause }

// Program cancellation markers are mechanisms, not independent failures. Only
// a complete bounded tree consisting of those markers may be normalized.
func cancellationOnly(err error, status error, program bool) (pure bool) {
	defer func() {
		if recover() != nil {
			pure = false
		}
	}()
	remaining := 32
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil || remaining == 0 {
			return false
		}
		remaining--
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
			if value.IsNil() {
				return false
			}
		}
		if current == status || (program && current == tea.ErrProgramKilled) {
			return true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > remaining {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap())
		}
		return false
	}
	return visit(err)
}

func programFailure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		status, cause := ctx.Err(), context.Cause(ctx)
		if cancellationOnly(err, status, true) && (cause == nil || cancellationOnly(cause, status, false)) {
			return status
		}
		err = errors.Join(err, status, cause)
	}
	return selectorFailure{cause: err}
}

// Bubble Tea's renderer ignores writer errors. Retain the first original
// failure through final terminal restoration without logging rendered content.
type selectorOutput struct {
	io.Writer
	mu  sync.Mutex
	err error
}

func (w *selectorOutput) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
	}
	return n, err
}
func (w *selectorOutput) failure() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }

func writeScreen(output io.Writer, sequence string) {
	n, err := io.WriteString(output, sequence)
	if err == nil && n != len(sequence) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// A public chooser/loading owner will return an error from its fenced
		// output; the void screen APIs have no return path and observe it here.
		if _, owned := output.(*selectorOutput); !owned {
			observeFailure(context.Background(), err)
		}
	}
}
func observeFailure(ctx context.Context, err error) {
	if err == nil || errorreport.HTTPAttemptObserved(err) {
		return
	}
	fault := errorreport.ProjectFault(ctx, "paperboat-cli", "command", "lifecycle", "local_access_failed", err)
	if fault.Outcome != "canceled" {
		errorreport.Current().ObserveFailure(ctx, "paperboat-cli", "command", "lifecycle", "local_access_failed", err)
	}
}

// Tea normally catches command panics. The explicitly joined loading worker
// retains that boundary without printing an arbitrary panic payload.
func loadingWork(ctx context.Context, work func(context.Context) error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			if cause, ok := value.(error); ok {
				err = selectorFailure{cause: errors.Join(tea.ErrProgramPanic, cause)}
			} else {
				err = tea.ErrProgramPanic
			}
		}
	}()
	return work(ctx)
}
