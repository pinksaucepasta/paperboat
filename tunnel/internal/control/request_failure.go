package control

// RequestFailure carries only a compile-time control path, status and fixed
// category. Response bodies, credentials and transport error text are excluded.
type RequestFailure struct {
	Path             string
	Status           int
	Category         string
	SupportReference string
	Err              error
	Cause            error
}

func (e *RequestFailure) Error() string   { return "control request failed" }
func (e *RequestFailure) Unwrap() []error { return []error{e.Err, e.Cause} }
func (e *RequestFailure) DiagnosticStatus() int {
	if e == nil {
		return 0
	}
	return e.Status
}

// documentFailure retains a decoder/parser cause without retaining private
// input in the public message. Unlike RequestFailure, its semantic validation
// has not already been published by the HTTP attempt owner.
type documentFailure struct {
	sentinel error
	cause    error
}

func (e documentFailure) Error() string   { return "control document is invalid" }
func (e documentFailure) Unwrap() []error { return []error{e.sentinel, e.cause} }
