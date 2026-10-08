package control

// RequestFailure carries only a compile-time control path, status and fixed
// category. Response bodies, credentials and transport error text are excluded.
type RequestFailure struct {
	Path     string
	Status   int
	Category string
	Err      error
}

func (e *RequestFailure) Error() string { return "control request failed" }
func (e *RequestFailure) Unwrap() error { return e.Err }
