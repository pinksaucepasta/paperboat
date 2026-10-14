package hostinstall

// suppliedRuntimeSourceFailure preserves the invalid-installation policy and
// the independent verification cause without rendering paths or source errors.
type suppliedRuntimeSourceFailure struct{ cause error }

func (suppliedRuntimeSourceFailure) Error() string {
	return "supplied runtime bytes could not be verified"
}
func (e suppliedRuntimeSourceFailure) Unwrap() []error { return []error{ErrInvalidRequest, e.cause} }
