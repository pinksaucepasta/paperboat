package browserbroadcastserver

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

const (
	streamOpenStage = "stream_open"
	deliveryStage   = "delivery"
	lifecycleStage  = "lifecycle"
	transportCode   = "transport_failed"
	sessionCode     = "terminal_session_failed"
)

var errOutputIndexExhausted = errors.New("browser terminal output sequence exhausted")

type diagnosticFailure struct {
	stage string
	code  string
	cause error
}

func (e *diagnosticFailure) Error() string {
	switch e.stage {
	case streamOpenStage:
		return "browser terminal output stream setup failed"
	case deliveryStage:
		return "browser terminal output delivery failed"
	default:
		return "browser terminal output cleanup failed"
	}
}

func (e *diagnosticFailure) Unwrap() error           { return e.cause }
func (e *diagnosticFailure) DiagnosticStage() string { return e.stage }
func (e *diagnosticFailure) DiagnosticCode() string  { return e.code }

func classifiedFailure(err error, stage, code string) error {
	if err == nil {
		return nil
	}
	return &diagnosticFailure{stage: stage, code: code, cause: err}
}

func reportPublisherFailure(ctx context.Context, stage string, err error) {
	if err == nil || expectedPublisherEnd(err) {
		return
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "sessions", stage, sessionCode, classifiedFailure(err, stage, sessionCode))
}

func reportPublisherCleanupFailure(ctx context.Context, err error) {
	if err == nil || expectedPublisherEnd(err) {
		return
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "sessions", lifecycleStage, sessionCode, classifiedFailure(err, lifecycleStage, sessionCode))
}

func expectedPublisherEnd(err error) bool {
	return allErrorLeavesMatch(err, func(leaf error) bool {
		if normalPublisherTermination(leaf) {
			return true
		}
		return leaf == session.ErrSessionUnknown || leaf == session.ErrAttachmentUnknown
	})
}

func normalPublisherTermination(err error) bool {
	return err == context.Canceled || err == io.EOF || err == io.ErrClosedPipe || err == os.ErrClosed || err == net.ErrClosed
}

// allErrorLeavesMatch is bounded and conservative. A cyclic, typed-nil, or
// oversized error cannot establish that a publisher stopped normally.
func allErrorLeavesMatch(err error, match func(error) bool) bool {
	if err == nil || match == nil {
		return false
	}
	queue := []error{err}
	seen := make(map[error]struct{})
	visited := 0
	leaves := 0
	for len(queue) > 0 {
		if visited >= 16 {
			return false
		}
		current := queue[0]
		queue = queue[1:]
		visited++
		if current == nil {
			return false
		}
		typeOf := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if typeOf.Comparable() {
			if _, exists := seen[current]; exists {
				return false
			}
			seen[current] = struct{}{}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(queue)+len(children) > 16-visited {
				return false
			}
			queue = append(queue, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(queue)+1 > 16-visited {
				return false
			}
			queue = append(queue, child)
		default:
			leaves++
			if !match(current) {
				return false
			}
		}
	}
	return leaves > 0
}
