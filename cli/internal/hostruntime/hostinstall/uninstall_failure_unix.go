//go:build darwin || linux

package hostinstall

// Native cleanup stages cross the privileged process boundary as finite safe
// classifications. The original cause remains available to its owning process.
type uninstallFailure struct {
	stage string
	cause error
}

func (e uninstallFailure) Error() string           { return "Paperboat uninstall failed at " + e.stage }
func (e uninstallFailure) Unwrap() error           { return e.cause }
func (e uninstallFailure) DiagnosticStage() string { return e.stage }
func (e uninstallFailure) DiagnosticCode() string  { return "purge_" + e.stage + "_failed" }

func withUninstallStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return uninstallFailure{stage: stage, cause: err}
}
