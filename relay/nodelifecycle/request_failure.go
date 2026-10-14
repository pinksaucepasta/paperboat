package nodelifecycle

// RequestFailure retains the original typed cause and authority decision while
// excluding request URLs, credential values and response text from Error.
type RequestFailure struct {
	Err    error
	Status int
	fenced bool
}

func (e *RequestFailure) Error() string {
	if e.fenced {
		return ErrFenced.Error()
	}
	return ErrControl.Error()
}
func (e *RequestFailure) DiagnosticStatus() int { return e.Status }
func (e *RequestFailure) Unwrap() []error {
	authority := ErrControl
	if e.fenced {
		authority = ErrFenced
	}
	return []error{authority, e.Err}
}
func failure(err error, status int, fenced bool) error {
	return &RequestFailure{Err: err, Status: status, fenced: fenced}
}
