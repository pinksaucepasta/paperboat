package machineguard

// Cleanup operation identifiers are the finite uninstall diagnostic contract.
const (
	CleanupPrecondition        = "guard_precondition"
	CleanupStateInspect        = "guard_state_inspect"
	CleanupUninstall           = "guard_uninstall"
	CleanupTaskRemove          = "guard_task_remove"
	CleanupProcessStop         = "guard_process_stop"
	CleanupWFPRemove           = "guard_wfp_remove"
	CleanupHostsCleanup        = "guard_hosts_cleanup"
	CleanupCertificatesCleanup = "guard_certificates_cleanup"
	CleanupPayloadRemove       = "guard_payload_remove"
	CleanupStateRemove         = "guard_state_remove"
	CleanupPayloadParentRemove = "guard_payload_parent_remove"
	CleanupStateParentRemove   = "guard_state_parent_remove"

	MaxCleanupOperations = 12
)

// ValidCleanupOperation rejects identifiers outside the owning cleanup contract.
func ValidCleanupOperation(operation string) bool {
	switch operation {
	case CleanupPrecondition, CleanupStateInspect, CleanupUninstall, CleanupTaskRemove, CleanupProcessStop, CleanupWFPRemove, CleanupHostsCleanup, CleanupCertificatesCleanup, CleanupPayloadRemove, CleanupStateRemove, CleanupPayloadParentRemove, CleanupStateParentRemove:
		return true
	default:
		return false
	}
}

// cleanupOperationError identifies a finite cleanup operation without exposing
// its paths or native error text to the uninstall status consumer.
type cleanupOperationError struct {
	operation string
	cause     error
}

func (e *cleanupOperationError) Error() string            { return e.cause.Error() }
func (e *cleanupOperationError) Unwrap() error            { return e.cause }
func (e *cleanupOperationError) CleanupOperation() string { return e.operation }

func cleanupFailure(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return &cleanupOperationError{operation: operation, cause: cause}
}
