package runtime

import "fmt"

// StartError identifies a local component without rendering the wrapped error,
// which may contain credentials or private remote response data.
type StartError struct {
	Component string
	Err       error
}

func (e *StartError) Error() string { return "start component " + e.Component + " failed" }
func (e *StartError) Unwrap() error { return e.Err }
func componentStartError(component Component, err error) error {
	return &StartError{Component: fmt.Sprintf("%T", component), Err: err}
}
