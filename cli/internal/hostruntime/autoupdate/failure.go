package autoupdate

import (
	"context"
	"os"
	"reflect"
	"syscall"
)

type updateError struct {
	message string
	causes  []error
}

func (e *updateError) Error() string   { return e.message }
func (e *updateError) Unwrap() []error { return e.causes }
func updateFailure(message string, causes ...error) error {
	retained := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			retained = append(retained, cause)
		}
	}
	return &updateError{message: message, causes: retained}
}

// Expected outcomes require a complete bounded cause tree. A joined native or
// unresolved failure must never authorize absence fallback or quiet blocking.
func updateLeavesOnly(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	seen := map[error]bool{}
	leaves := 0
	for nodes := 0; len(pending) > 0; nodes++ {
		if nodes >= 32 {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
			if value.IsNil() {
				return false
			}
		}
		if match(current) {
			leaves++
			continue
		}
		if value.Comparable() {
			if seen[current] {
				return false
			}
			seen[current] = true
		}
		switch unwrap := current.(type) {
		case interface{ Unwrap() []error }:
			children := unwrap.Unwrap()
			if len(children) == 0 {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := unwrap.Unwrap()
			if child == nil {
				return false
			}
			pending = append(pending, child)
		default:
			return false
		}
	}
	return leaves > 0
}
func updateCausesOnly(err, target error) bool {
	return updateLeavesOnly(err, func(cause error) bool {
		if reflect.TypeOf(cause).Comparable() && cause == target {
			return true
		}
		if native, ok := cause.(syscall.Errno); ok && (target == os.ErrNotExist || target == os.ErrPermission || target == os.ErrExist) {
			return native.Is(target)
		}
		return false
	})
}
func expectedUpdateBlock(err error) (*ActiveTerminalSessionsError, *OwnerMaintenancePendingError) {
	var active *ActiveTerminalSessionsError
	var maintenance *OwnerMaintenancePendingError
	if !updateLeavesOnly(err, func(cause error) bool {
		switch leaf := cause.(type) {
		case *ActiveTerminalSessionsError:
			if active != nil && active != leaf {
				return false
			}
			active = leaf
			return maintenance == nil
		case *OwnerMaintenancePendingError:
			if maintenance != nil && maintenance != leaf {
				return false
			}
			maintenance = leaf
			return active == nil
		}
		return false
	}) {
		return nil, nil
	}
	return active, maintenance
}
func updateContextFailure(ctx context.Context) error {
	primary := ctx.Err()
	cause := context.Cause(ctx)
	if cause == nil || (reflect.TypeOf(cause).Comparable() && cause == primary) {
		return primary
	}
	return updateFailure("automatic update check was interrupted", primary, cause)
}
