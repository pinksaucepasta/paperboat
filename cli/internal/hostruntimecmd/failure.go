package hostruntimecmd

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
)

type runtimeCommandFailure struct {
	message string
	causes  []error
}

func (e *runtimeCommandFailure) Error() string {
	if e == nil {
		return "host runtime command failed"
	}
	return e.message
}
func (e *runtimeCommandFailure) Unwrap() []error {
	if e == nil {
		return nil
	}
	return e.causes
}

// Messages are fixed prose authored by this package. Causes stay private and
// inspectable by the final invocation owner.
func runtimeFailure(message string, causes ...error) error {
	retained := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			retained = append(retained, cause)
		}
	}
	return &runtimeCommandFailure{message: message, causes: retained}
}

func runtimeContextFailure(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
		return errors.Join(ctx.Err(), cause)
	}
	return ctx.Err()
}

// Complete, bounded proof is required before absence, cancellation, or a
// domain outcome may authorize recovery or suppress a terminal fault.
func runtimeCausesOnly(err error, allowed ...error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	seen := map[error]bool{}
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 32 {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() {
			if seen[current] {
				return false
			}
			seen[current] = true
		}

		// Exact package-owned sentinel identity is authoritative, including
		// static sentinels which wrap another domain sentinel. Sibling causes
		// are still checked independently; arbitrary Is hooks are never used.
		sentinel := false
		if value.Type().Comparable() {
			for _, candidate := range allowed {
				if current == candidate {
					sentinel = true
					break
				}
			}
		}
		if sentinel {
			continue
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			if child := wrapped.Unwrap(); child != nil {
				pending = append(pending, child)
				continue
			}
		}
		matched := false
		for _, candidate := range allowed {
			if value.Type().Comparable() && current == candidate {
				matched = true
				break
			}
			if native, ok := current.(syscall.Errno); ok {
				switch candidate {
				case os.ErrNotExist, os.ErrPermission, os.ErrExist:
					if native.Is(candidate) {
						matched = true
					}
				}
				if matched {
					break
				}
			}

		}
		if !matched {
			return false
		}
	}
	return true
}

func observeRuntimeFailure(ctx context.Context, operation string, err error) {
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", operation, "command", "command_failed", err)
	}
}

func runtimeFailureIfAny(message string, causes ...error) error {
	for _, cause := range causes {
		if cause != nil {
			return runtimeFailure(message, causes...)
		}
	}
	return nil
}

type runtimeHTTPFailure struct{ status int }

func (runtimeHTTPFailure) Error() string           { return "host runtime HTTP request failed" }
func (e runtimeHTTPFailure) DiagnosticStatus() int { return e.status }

// Already reported component failures are pruned only at their authoritative
// producer boundary. Independent cleanup remains owned by the command fence.
func unreportedRuntimeFailure(err error) error {
	seen := map[error]bool{}
	remaining := 32
	ownedCount := 0
	var visit func(error) (error, bool)
	visit = func(current error) (error, bool) {
		remaining--
		if remaining < 0 || current == nil {
			return nil, false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
			if value.IsNil() {
				return nil, false
			}
		}
		if value.Type().Comparable() {
			if seen[current] {
				return nil, false
			}
			seen[current] = true
		}
		if owned, ok := current.(interface{ RuntimeLifecycleFailureOwned() bool }); ok && owned.RuntimeLifecycleFailureOwned() {
			ownedCount++
			return nil, true
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return current, true
			}
			before := ownedCount
			pending := make([]error, 0, len(children))
			for _, child := range children {
				unowned, valid := visit(child)
				if !valid {
					return nil, false
				}
				if unowned != nil {
					pending = append(pending, unowned)
				}
			}
			if before == ownedCount {
				return current, true
			}
			if failure, ok := current.(*runtimeCommandFailure); ok && len(pending) > 0 {
				return runtimeFailure(failure.message, pending...), true
			}
			return errors.Join(pending...), true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			if child := wrapped.Unwrap(); child != nil {
				before := ownedCount
				unowned, valid := visit(child)
				if valid && before == ownedCount {
					return current, true
				}
				return unowned, valid
			}
		}
		return current, true
	}
	unowned, valid := visit(err)
	if !valid {
		return err
	}
	return unowned
}

func readRuntimeToken(file *os.File) (token []byte, resultErr error) {
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = withCleanup("closing the runtime token file failed", resultErr, err)
			token = nil
		}
	}()
	token, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil {
		return nil, runtimeFailure("reading the runtime token file failed", err)
	}
	if len(token) != 32 {
		return nil, runtimeFailure("runtime token file has an invalid size")
	}
	return token, nil
}

func runtimeShutdownCause(ctx context.Context) error {
	cause := runtimeContextFailure(ctx)
	if runtimeCausesOnly(cause, context.Canceled) {
		return nil
	}
	return cause
}

// FailureMessage returns the innermost authored runtime or installation
// failure message. These messages are fixed prose without paths, process
// output or identities, so the final command owner may present them.
func FailureMessage(err error) (string, bool) {
	if message, ok := hostinstall.FailureMessage(err); ok {
		return message, true
	}
	var failure *runtimeCommandFailure
	if !errors.As(err, &failure) || failure == nil || failure.message == "" {
		return "", false
	}
	for len(failure.causes) > 0 {
		var inner *runtimeCommandFailure
		if !errors.As(failure.causes[0], &inner) || inner == nil || inner.message == "" {
			break
		}
		failure = inner
	}
	return failure.message, true
}

// withCleanup attaches cleanup failures to a primary result without
// relabeling it. A successful cleanup returns the primary error unchanged;
// when both fail, the primary stays first so its message is presented.
func withCleanup(message string, primary error, cleanups ...error) error {
	cleanup := runtimeFailureIfAny(message, cleanups...)
	if cleanup == nil {
		return primary
	}
	if primary == nil {
		return cleanup
	}
	return errors.Join(primary, cleanup)
}

type installationStageError struct {
	Stage string
	Cause error
}

func (e *installationStageError) Error() string {
	step := "an internal installation step"
	if e != nil {
		switch e.Stage {
		case "artifact_verification":
			step = "verifying the runtime artifact"
		case "service_install":
			step = "installing system services"
		case "service_readiness":
			step = "confirming system service readiness"
		case "daemon_readiness":
			return "Machine pairing failed while binding the local daemon. The committed runtime was retained; retry pairing with the same enrollment token file."
		case "environment_readiness":
			return "Machine pairing failed while preparing managed environment variables. The committed runtime was retained; unlock your ENV vault to deliver them."
		}
	}
	return "Machine pairing failed while " + step + ". Run pb doctor, then retry pairing with the same enrollment token file."
}
func (e *installationStageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
