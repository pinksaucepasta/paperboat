package environmentmanager

import (
	"errors"
	"net/http"
	"os"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

var (
	ErrVariableNotConfigured = errors.New("environment variable is not configured")
	ErrAuthorityFork         = errors.New("ENV authority history conflicts with local state")
	ErrIntegrity             = errors.New("ENV encrypted state failed verification")
)

// vaultFailureLeavesOnly proves an expected absence only when every bounded
// terminal cause agrees. Mixed failures, cycles, typed nils, and oversized
// trees cannot authorize treating custody or a remote resource as missing.
func vaultFailureLeavesOnly(err error, matches func(error) bool) bool {
	if err == nil || matches == nil {
		return false
	}
	pending := []error{err}
	leaves := 0
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
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(pending)+len(children) > 16-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil {
				if !matches(current) {
					return false
				}
				leaves++
				continue
			}
			if len(pending)+1 > 16-visited {
				return false
			}
			pending = append(pending, child)
		default:
			if !matches(current) {
				return false
			}
			leaves++
		}
	}
	return leaves > 0
}

func vaultCredentialAbsentOnly(err error) bool {
	return vaultFailureLeavesOnly(err, func(leaf error) bool {
		return leaf == config.ErrSecretNotFound || os.IsNotExist(leaf)
	})
}

func vaultAPIResourceAbsentOnly(err error) bool {
	return vaultFailureLeavesOnly(err, func(leaf error) bool {
		status, ok := leaf.(interface{ DiagnosticStatus() int })
		return ok && status.DiagnosticStatus() == http.StatusNotFound
	})
}
