package session

import (
	"context"
	"io"
	"net"
	"os"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type sessionFailure struct {
	stage string
	cause error
}

func (e *sessionFailure) Error() string {
	switch e.stage {
	case "command":
		return "terminal process operation failed"
	case "delivery":
		return "terminal output delivery failed"
	default:
		return "terminal session lifecycle failed"
	}
}

func (e *sessionFailure) Unwrap() error { return e.cause }

func (e *sessionFailure) DiagnosticStage() string { return e.stage }

func (*sessionFailure) DiagnosticCode() string { return "terminal_session_failed" }

func classifySessionFailure(stage string, err error) error {
	if err == nil || onlyContextEnd(err) {
		return err
	}
	return &sessionFailure{stage: stage, cause: err}
}

func onlyContextEnd(err error) bool {
	return allSessionErrorLeaves(err, func(leaf error) bool {
		return leaf == context.Canceled || leaf == context.DeadlineExceeded
	})
}

func normalSessionTermination(err error) bool {
	return allSessionErrorLeaves(err, func(leaf error) bool {
		return leaf == io.EOF || leaf == os.ErrClosed || leaf == net.ErrClosed || leaf == context.Canceled
	})
}

func allSessionErrorLeaves(err error, isNormal func(error) bool) bool {
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
			if !isNormal(current) {
				return false
			}
		}
	}
	return leaves > 0
}

func observeSessionFailure(session *managedSession, stage string, err error) {
	if err == nil || normalSessionTermination(err) {
		return
	}
	ctx := supportref.WithContext(context.Background(), sessionSupportReference(session))
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "sessions", stage, "terminal_session_failed", err)
}

func setSessionSupportReference(session *managedSession, reference string) {
	if reference == "" {
		return
	}
	value := reference
	session.supportReference.Store(&value)
}

func sessionSupportReference(session *managedSession) string {
	reference := session.supportReference.Load()
	if reference == nil {
		return ""
	}
	return *reference
}
