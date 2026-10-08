package main

import (
	"context"
	"errors"
	"reflect"

	tea "github.com/charmbracelet/bubbletea"
)

// homeProgramFailure removes only Bubble Tea's cancellation mechanism marker
// when all causes agree with the caller's cancellation. Independent failures
// retain the original program error and the caller's cancellation cause.
func homeProgramFailure(ctx context.Context, err error) error {
	if err == nil || ctx == nil || ctx.Err() == nil {
		return err
	}
	status, cause := ctx.Err(), context.Cause(ctx)
	if pureHomeProgramCancellation(err, status) && (cause == nil || pureHomeProgramCancellation(cause, status)) {
		return status
	}
	if cause == nil || cause == status {
		return errors.Join(err, status)
	}
	return errors.Join(err, status, cause)
}

func pureHomeProgramCancellation(err, status error) (pure bool) {
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
		if current == tea.ErrProgramKilled || current == status {
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
		if wrapper, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapper.Unwrap())
		}
		return false
	}
	return visit(err)
}
