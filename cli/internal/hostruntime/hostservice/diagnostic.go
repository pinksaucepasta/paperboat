package hostservice

import (
	"context"
	"io"
	"net"
	"os"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

type operationFailure struct {
	stage string
	code  string
	err   error
}

func (e *operationFailure) Error() string {
	if e == nil {
		return "host service operation failed"
	}
	switch e.code {
	case "availability_apply_failed":
		return "host availability policy failed"
	case "diagnostic_storage_unavailable":
		return "host service state storage unavailable"
	case "managed_ssh_failed":
		return "managed SSH authorization reconciliation failed"
	case "peer_authority_failed":
		return "host service peer authorization check failed"
	default:
		return "host service operation failed"
	}
}

func (e *operationFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *operationFailure) DiagnosticStage() string { return e.stage }
func (e *operationFailure) DiagnosticCode() string  { return e.code }

func failAt(stage, code string, err error) error {
	if err == nil {
		return nil
	}
	return &operationFailure{stage: stage, code: code, err: err}
}

// allErrorLeavesMatch is intentionally small and bounded. It treats cycles,
// typed nils, and error trees larger than sixteen nodes as unexpected.
func allErrorLeavesMatch(err error, match func(error) bool) bool {
	if err == nil || match == nil {
		return false
	}
	pending := []error{err}
	seen := make(map[error]struct{})
	visited, leaves := 0, 0
	for len(pending) != 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		visited++
		if visited > 16 {
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

		var children []error
		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			children = multi.Unwrap()
		} else if unary, ok := current.(interface{ Unwrap() error }); ok {
			if child := unary.Unwrap(); child != nil {
				children = []error{child}
			}
		}
		nonNil := 0
		for _, child := range children {
			if child != nil {
				nonNil++
			}
		}
		if nonNil != 0 {
			if visited+len(pending)+nonNil > 16 {
				return false
			}
			for _, child := range children {
				if child != nil {
					pending = append(pending, child)
				}
			}
			continue
		}
		leaves++
		if !match(current) {
			return false
		}
	}
	return leaves > 0
}

func normalRequestTermination(err error) bool {
	return allErrorLeavesMatch(err, func(leaf error) bool {
		return normalRequestLeaf(leaf)
	})
}

func expectedRequestFailure(err error) bool {
	return allErrorLeavesMatch(err, func(leaf error) bool {
		return leaf == ErrPeerDenied || leaf == ErrInvalidRequest || leaf == ErrStalePolicy || normalRequestLeaf(leaf)
	})
}

func normalRequestLeaf(leaf error) bool {
	return leaf == context.Canceled || leaf == context.DeadlineExceeded || leaf == io.EOF || leaf == io.ErrClosedPipe || leaf == net.ErrClosed || leaf == os.ErrClosed || isPeerClosedError(leaf)
}

func captureUnexpected(ctx context.Context, operation, stage, code string, err error) {
	if err == nil || normalRequestTermination(err) {
		return
	}
	errorreport.Current().CaptureFailure(ctx, "paperboatd", operation, stage, code, err)
}

func observeUnexpected(ctx context.Context, operation, stage, code string, err error) {
	if err == nil || expectedRequestFailure(err) {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboatd", operation, stage, code, err)
}
