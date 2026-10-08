package localapi

import "reflect"

type permissionFailure struct{ cause error }

func (*permissionFailure) Error() string        { return "peer authority was denied" }
func (e *permissionFailure) Unwrap() error      { return e.cause }
func (*permissionFailure) Is(target error) bool { return target == ErrPermission }

// PermissionFailure retains the original cause after its transport owner has
// proven an authorization rejection. Operational failures must not use it.
func PermissionFailure(cause error) error { return &permissionFailure{cause: cause} }

// IsPermissionFailure accepts only a sole proven rejection. An independent
// joined failure must remain diagnosable even when errors.Is sees ErrPermission.
func IsPermissionFailure(err error) bool {
	for depth := 0; err != nil && depth < 8; depth++ {
		value := reflect.ValueOf(err)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() && err == ErrPermission {
			return true
		}
		if _, proven := err.(*permissionFailure); proven {
			return true
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}
