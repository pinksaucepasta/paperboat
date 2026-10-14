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
	if err == nil || isUpstreamAuthorityError(err) {
		return err
	}
	return &authorityFailure{cause: err}
}

func isUpstreamAuthorityError(err error) (upstream bool) {
	defer func() {
		if recover() != nil {
			upstream = false
		}
	}()
	if err == nil {
		return false
	}
	typeOf, targetType := reflect.TypeOf(err), reflect.TypeOf(ErrUpstream)
	if typeOf == targetType && typeOf.Comparable() && err == ErrUpstream {
		return true
	}
	if classified, ok := err.(interface{ Is(error) bool }); ok {
		return classified.Is(ErrUpstream)
	}
	return false
}

func httpAttemptWasObserved(err error) (observed bool) {
	defer func() {
		if recover() != nil {
			observed = false
		}
	}()
	return errorreport.HTTPAttemptObserved(err)
}

func expectedAuthorityOutcome(err error) bool {
	if err == nil {
		return false
	}
	classifiedDenial := false
	allExpected := allErrorLeavesMatch(err, func(leaf error) bool {
		if leaf == ErrDenied || leaf == ErrInvalid {
			classifiedDenial = true
			return true
		}
		if leaf == context.Canceled {
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
	if !allExpected {
		return false
	}
	if classifiedDenial {
		return true
	}
	return allErrorLeavesMatch(err, func(leaf error) bool { return leaf == context.Canceled })
}

// allErrorLeavesMatch is bounded and conservative. A cyclic, typed-nil, or
// oversized error cannot establish an intentional denial or cancellation.
func allErrorLeavesMatch(err error, match func(error) bool) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
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
		if !typeOf.Comparable() {
			return false
		}
		value := reflect.ValueOf(current)
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
