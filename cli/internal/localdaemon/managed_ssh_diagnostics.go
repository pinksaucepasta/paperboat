package localdaemon

import (
	"context"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

type managedSSHAuthorityFailure struct{ cause error }

func (e *managedSSHAuthorityFailure) Error() string {
	if e == nil {
		return ""
	}
	return "managed SSH authority operation failed"
}

func (e *managedSSHAuthorityFailure) Unwrap() error { return e.cause }

func (*managedSSHAuthorityFailure) DiagnosticStage() string { return "peer_authority" }

func (*managedSSHAuthorityFailure) DiagnosticCode() string { return "managed_ssh_failed" }

func managedSSHAuthorityError(err error) error {
	if err == nil {
		return nil
	}
	return &managedSSHAuthorityFailure{cause: err}
}

func managedSSHHealthCode(err error) string {
	if err == nil {
		return ""
	}
	if managedSSHAuthenticationRejection(err) {
		return "ssh_key_rejected"
	}
	if hasAPIErrorCode(err, "managed_ssh_client_key_capacity") {
		return managedSSHClientKeyCapacityCode
	}
	return "ssh_target_not_ready"
}

// hasAPIErrorCode walks at most 16 causes (errors.As would loop on a cyclic chain, see BoundsCyclicCause).
func hasAPIErrorCode(err error, code string) bool {
	pending := []error{err}
	for visited := 0; len(pending) > 0 && visited < 16; visited++ {
		current := pending[0]
		pending = pending[1:]
		if response, ok := current.(*api.APIError); ok && response != nil && response.Code == code {
			return true
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, wrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return false
}

// managedSSHClientKeyCapacityCode: the account already has the most signed-in computers using managed SSH, so this
// computer's key was refused. Retrying cannot help until one is signed out (pb logout) or removed.
const managedSSHClientKeyCapacityCode = "ssh_client_key_capacity"

func managedSSHAuthenticationRejection(err error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	seen := make(map[error]struct{})
	unauthenticatedLeaves := 0
	hasHTTPRejection := false
	joined := false
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
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
		statusRejection := false
		if status, ok := current.(interface{ DiagnosticStatus() int }); ok {
			switch status.DiagnosticStatus() {
			case 401, 403:
				statusRejection = true
				hasHTTPRejection = true
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			joined = true
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 16-visited-1 {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil {
				if typeOf.Comparable() && current == api.ErrUnauthenticated {
					unauthenticatedLeaves++
				} else if !statusRejection {
					return false
				}
				continue
			}
			if len(pending)+1 > 16-visited-1 {
				return false
			}
			pending = append(pending, child)
		default:
			if typeOf.Comparable() && current == api.ErrUnauthenticated {
				unauthenticatedLeaves++
			} else if typeOf.Comparable() && current == context.Canceled {
				// ProjectFault treats caller cancellation alongside a rejected
				// response as expected. It still rejects mixed operational joins.
			} else if !statusRejection {
				return false
			}
		}
	}
	if hasHTTPRejection {
		fault := errorreport.ProjectFault(context.Background(), "paperboatd", "ssh", "peer_authority", "managed_ssh_failed", err)
		return fault.Outcome == "rejected" && (fault.HTTPStatus == 401 || fault.HTTPStatus == 403)
	}
	return !joined && unauthenticatedLeaves == 1
}
