package envinject

import (
	"errors"
	"reflect"
)

const (
	environmentReconciliationStage = "reconciliation"
	environmentStorageStage        = "diagnostic_storage"
	environmentSyncFailureCode     = "environment_sync_failed"
)

// operationFailure carries only bounded classification across the package
// boundary. The original cause remains available through Unwrap, while its
// text is kept out of user-facing errors and diagnostics.
type operationFailure struct {
	stage string
	err   error
}

func (e *operationFailure) Error() string {
	if e == nil {
		return "environment operation failed"
	}
	if e.stage == environmentStorageStage {
		return "environment state storage failed"
	}
	return "environment synchronization failed"
}

func (e *operationFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *operationFailure) DiagnosticStage() string {
	if e == nil {
		return ""
	}
	return e.stage
}

func (*operationFailure) DiagnosticCode() string { return environmentSyncFailureCode }

func classifyOperation(err error, stage string) error {
	if err == nil || hasDiagnosticClassification(err) {
		return err
	}
	return &operationFailure{stage: stage, err: err}
}

func hasDiagnosticClassification(err error) bool {
	var classified interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if !errors.As(err, &classified) || classified == nil {
		return false
	}
	value := reflect.ValueOf(classified)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return false
	}
	return classified.DiagnosticStage() != "" && classified.DiagnosticCode() != ""
}
