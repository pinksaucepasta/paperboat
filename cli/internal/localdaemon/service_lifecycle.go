package localdaemon

import "errors"

// ErrServiceNotInstalled means the local daemon's service is not registered with the operating
// system. Commands name pb service install, which registers it again.
var ErrServiceNotInstalled = errors.New("Paperboat local daemon service is not installed; run pb service install")

// ServiceState is the canonical local daemon's native service-manager state.
type ServiceState struct {
	Installed bool
	Running   bool
}

// AdministratorRequiredError means a service action needs administrator rights (Windows) and the
// user did not give them: the prompt was declined or could not be shown. Reason, when set, is the
// full message (a terminal that cannot start the helper has its own advice).
type AdministratorRequiredError struct {
	Cause  error
	Reason string
}

func (e *AdministratorRequiredError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "This needs administrator rights. Approve the Windows prompt, or run the command from an administrator terminal."
}

func (e *AdministratorRequiredError) Unwrap() error { return e.Cause }
