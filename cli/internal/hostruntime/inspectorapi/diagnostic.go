package inspectorapi

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

const (
	peerAuthorityStage = "peer_authority"
	peerAuthorityCode  = "peer_authority_failed"
)

var errInvalidAuthorityDecision = errors.New("inspector authority decision is invalid")

type authorityFailure struct{ cause error }

func (e *authorityFailure) Error() string           { return "inspector authority is unavailable" }
func (e *authorityFailure) Unwrap() error           { return e.cause }
func (e *authorityFailure) Is(target error) bool    { return target == ErrUpstream }
func (e *authorityFailure) DiagnosticStage() string { return peerAuthorityStage }
func (e *authorityFailure) DiagnosticCode() string  { return peerAuthorityCode }

func invalidAuthorityDecision() error {
	return &authorityFailure{cause: errInvalidAuthorityDecision}
}

func unexpectedAuthorityFailure(err error) error {
	if err == nil || errors.Is(err, ErrUpstream) {
		return err
	}
	return &authorityFailure{cause: err}
}

func observeAuthorityFailure(ctx context.Context, err error) {
	if err == nil || expectedAuthorityOutcome(err) || errorreport.HTTPAttemptObserved(err) {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "inspector_authorization", peerAuthorityStage, peerAuthorityCode, err)
}

func expectedAuthorityOutcome(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrDenied) || errors.Is(err, ErrInvalid) {
		return allErrorLeavesMatch(err, func(leaf error) bool {
			if leaf == ErrDenied || leaf == ErrInvalid || leaf == context.Canceled {
				return true
			}
			status, ok := leaf.(interface{ DiagnosticStatus() int })
			if !ok {
				return false
			}
			switch status.DiagnosticStatus() {
			case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
				return true
			default:
				return false
			}
		})
	}
	return allErrorLeavesMatch(err, func(leaf error) bool { return leaf == context.Canceled })
}

// allErrorLeavesMatch is bounded and conservative. A cyclic, typed-nil, or
// oversized error cannot establish an intentional denial or cancellation.
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
