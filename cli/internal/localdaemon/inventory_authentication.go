package localdaemon

import (
	"context"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

// Only proven authentication failures may discard the last usable inventory.
// A rejection joined with an operational failure remains degraded and retryable.
func inventoryAuthenticationRequired(err error) bool {
	if err == nil {
		return false
	}
	fault := errorreport.ProjectFault(context.Background(), "paperboatd", "machine_control", "control_request", "control_request_failed", err)
	if fault.HTTPStatus != 0 {
		return fault.HTTPStatus == 401 && fault.Outcome == "rejected"
	}
	seen := make(map[error]struct{})
	for depth := 0; err != nil && depth < 16; depth++ {
		value := reflect.ValueOf(err)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() {
			if _, exists := seen[err]; exists {
				return false
			}
			seen[err] = struct{}{}
			if err == config.ErrNoCredentials || err == config.ErrSecretNotFound || err == api.ErrUnauthenticated {
				return true
			}
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
