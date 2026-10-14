package managedssh

// NativeExitError retains the native process status without incorporating its
// stderr or arguments into diagnostic text. Expected exits are not SDK faults.
type NativeExitError struct {
	Code int
	Err  error
}

func (e NativeExitError) Error() string { return "native SSH tool exited" }
func (e NativeExitError) ExitCode() int { return e.Code }
func (e NativeExitError) Unwrap() error { return e.Err }

// NativeLaunchError retains typed lookup/start errors while excluding paths and
// user-supplied argv from public error text.
type NativeLaunchError struct{ Err error }

func (e NativeLaunchError) Error() string { return "native SSH tool could not start" }
func (e NativeLaunchError) Unwrap() error { return e.Err }
