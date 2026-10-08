package config

import (
	"os"
	"reflect"
)

// safeConfigError keeps typed causes available to errors.Is/As and diagnostic
// projection while presenting only package-owned prose to callers. Messages
// passed to these constructors must be static strings.
type safeConfigError struct {
	message string
	causes  []error
}

func (err safeConfigError) Error() string { return err.message }

func (err safeConfigError) Unwrap() []error {
	causes := make([]error, len(err.causes))
	copy(causes, err.causes)
	return causes
}

func safeConfigCause(message string, cause error) error {
	if cause == nil {
		return safeConfigError{message: message}
	}
	return safeConfigError{message: message, causes: []error{cause}}
}

func isSafeConfigError(err error) bool {
	switch err.(type) {
	case safeConfigError, *safeConfigError:
		return true
	default:
		return false
	}
}

func credentialStoreFailure(message string, cause error) error {
	causes := []error{ErrCredentialStoreUnavailable}
	if cause != nil {
		causes = append(causes, cause)
	}
	return safeConfigError{message: message, causes: causes}
}

// credentialAbsenceOnly accepts a missing-secret or missing-file result only
// when every bounded terminal cause is one of those expected states. A joined
// I/O failure, typed nil, cycle, or oversized chain cannot authorize creating
// or replacing credential state.
func credentialAbsenceOnly(err error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	seen := make(map[error]struct{})
	sawAbsence := false
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
		if value.Kind() == reflect.Chan || value.Kind() == reflect.Func || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice {
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() {
			if _, exists := seen[current]; exists {
				if current == ErrSecretNotFound || os.IsNotExist(current) {
					sawAbsence = true
					continue
				}
				return false
			}
			seen[current] = struct{}{}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(pending)+len(children) > 15-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(pending) >= 15-visited {
				return false
			}
			pending = append(pending, child)
		default:
			if current != ErrSecretNotFound && !os.IsNotExist(current) {
				return false
			}
			sawAbsence = true
		}
	}
	return sawAbsence
}
