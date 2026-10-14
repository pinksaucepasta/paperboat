package hostdproto

import (
	"context"
	"io"
	"net"
	"os"
	"reflect"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

type socketOperationFailure struct {
	message string
	cause   error
}

func (failure *socketOperationFailure) Error() string { return failure.message }
func (failure *socketOperationFailure) Unwrap() error { return failure.cause }

// expectedSocketRequestFailure suppresses only bounded error trees made
// entirely of ordinary protocol rejection, cancellation, or peer disconnect
// leaves. A mixed tree remains observable.
func expectedSocketRequestFailure(err error) (expected bool) {
	defer func() {
		if recover() != nil {
			expected = false
		}
	}()
	if err == nil {
		return false
	}
	pending := []error{err}
	seen := make(map[error]struct{})
	leaves := 0
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if !value.Comparable() {
			return false
		}
		if _, consumedOperationFailure := current.(*socketOperationFailure); consumedOperationFailure {
			return false
		}
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if _, exists := seen[current]; exists {
			return false
		}
		seen[current] = struct{}{}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 16-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil {
				return false
			}
			pending = append(pending, child)
		default:
			leaves++
			if !expectedSocketRequestLeaf(current) {
				return false
			}
		}
	}
	return leaves > 0
}

func expectedSocketRequestLeaf(err error) bool {
	switch err {
	case context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe,
		os.ErrClosed, os.ErrDeadlineExceeded, net.ErrClosed,
		ErrInvalidFrame, ErrIncompatible, ErrFenced, ErrMaintenanceBusy, ErrNotReady:
		return true
	}
	if errno, ok := err.(syscall.Errno); ok {
		return errno == syscall.EPIPE || errno == syscall.ECONNRESET
	}
	return false
}

// socketErrorResponseCode maps only bounded, entirely expected protocol errors
// to their specific wire code. Operational and mixed errors use the fixed
// invalid code, and custom Unwrap implementations fail closed if they panic.
func socketErrorResponseCode(err error) (code string) {
	code = "invalid"
	if !expectedSocketRequestFailure(err) {
		return code
	}
	defer func() {
		if recover() != nil {
			code = "invalid"
		}
	}()
	if err == nil {
		return code
	}
	pending := []error{err}
	seen := make(map[error]struct{})
	selected := ""
	leaves := 0
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return code
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return code
		}
		value := reflect.ValueOf(current)
		if !value.Comparable() {
			return code
		}
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return code
			}
		}
		if _, exists := seen[current]; exists {
			return code
		}
		seen[current] = struct{}{}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 16-visited {
				return code
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil {
				return code
			}
			pending = append(pending, child)
		default:
			leafCode := socketProtocolErrorCode(current)
			if leafCode == "" || (selected != "" && selected != leafCode) {
				return code
			}
			selected = leafCode
			leaves++
		}
	}
	if leaves > 0 {
		return selected
	}
	return code
}

func socketProtocolErrorCode(err error) string {
	switch err {
	case ErrIncompatible:
		return "incompatible"
	case ErrFenced:
		return "fenced"
	case ErrMaintenanceBusy:
		return "maintenance_busy"
	case ErrNotReady:
		return "not_ready"
	default:
		return ""
	}
}

func observeSocketRequestFailure(ctx context.Context, err error) {
	if err == nil || expectedSocketRequestFailure(err) || expectedSocketRequestShutdownFailure(ctx, err) || socketHTTPAttemptObserved(err) {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboatd", "machine_control", "control_request", "control_request_failed", err)
}

func socketHTTPAttemptObserved(err error) (observed bool) {
	defer func() {
		if recover() != nil {
			observed = false
		}
	}()
	return errorreport.HTTPAttemptObserved(err)
}
