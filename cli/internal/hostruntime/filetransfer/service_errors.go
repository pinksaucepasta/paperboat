package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

type Code string

const (
	InvalidPath        Code = "invalid_path"
	InvalidSize        Code = "invalid_size"
	BatchLimit         Code = "batch_limit"
	OffsetConflict     Code = "offset_conflict"
	StateConflict      Code = "state_conflict"
	DigestMismatch     Code = "digest_mismatch"
	StorageUnavailable Code = "storage_unavailable"
	ResourceLimit      Code = "resource_limit"
	Canceled           Code = "canceled"
	DeliveryTimeout    Code = "delivery_timeout"
)

type Error struct {
	Code  Code
	Cause error
}

func (e *Error) Error() string {
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

func lookupFailure(err error) error {
	if onlyContextTermination(err) {
		return err
	}
	code := StorageUnavailable
	if errors.Is(err, store.ErrNotFound) {
		code = InvalidPath
	}
	return &Error{Code: code, Cause: err}
}

type cleanupFailure struct{ cause error }

func (*cleanupFailure) Error() string { return "file transfer retention cleanup failed" }

func (e *cleanupFailure) Unwrap() error { return e.cause }

func (*cleanupFailure) DiagnosticStage() string { return "lifecycle" }

func (*cleanupFailure) DiagnosticCode() string { return "file_transfer_failed" }

func classifyCleanupFailure(err error) error {
	if err == nil || onlyContextTermination(err) {
		return err
	}
	return &cleanupFailure{cause: err}
}

func onlyContextTermination(err error) bool {
	if err == nil {
		return false
	}
	remaining := []error{err}
	seen := make(map[error]struct{})
	leaves := 0
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		typeOf := reflect.TypeOf(current)
		if typeOf.Comparable() {
			if _, ok := seen[current]; ok {
				return false
			}
			seen[current] = struct{}{}
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children) > 16-visited-1 || len(remaining)+len(children) > 16-visited-1 {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(remaining)+1 > 16-visited-1 {
				return false
			}
			remaining = append(remaining, child)
		default:
			leaves++
			if current != context.Canceled && current != context.DeadlineExceeded {
				return false
			}
		}
	}
	return leaves > 0
}

func (e *Error) Format(state fmt.State, verb rune) { _, _ = fmt.Fprint(state, e.Error()) }
