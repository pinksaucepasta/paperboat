package localdaemon

import "errors"

type daemonCleanupError struct{ cause error }

func (daemonCleanupError) Error() string           { return "local daemon cleanup failed" }
func (e daemonCleanupError) Unwrap() error         { return e.cause }
func (daemonCleanupError) DiagnosticStage() string { return "component_shutdown" }
func (daemonCleanupError) DiagnosticCode() string  { return "service_failed" }

func daemonCleanupFailure(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	failure := daemonCleanupError{cleanup}
	if primary == nil {
		return failure
	}
	return errors.Join(primary, failure)
}
