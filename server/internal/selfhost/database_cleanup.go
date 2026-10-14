package selfhost

import "database/sql"

// databaseCleanupFailure keeps the operation and both failure causes while
// avoiding raw driver messages in ordinary error formatting.
type databaseCleanupFailure struct {
	operation string
	primary   error
	cleanup   error
}

func (e *databaseCleanupFailure) Error() string {
	return "self-host " + e.operation + " database cleanup failed"
}

func (e *databaseCleanupFailure) Unwrap() []error {
	causes := make([]error, 0, 2)
	if e.primary != nil {
		causes = append(causes, e.primary)
	}
	if e.cleanup != nil {
		causes = append(causes, e.cleanup)
	}
	return causes
}

func withDatabaseCleanupFailure(operation string, primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return &databaseCleanupFailure{operation: operation, primary: primary, cleanup: cleanup}
}

func withTransactionRollbackFailure(operation string, primary, rollback error) error {
	if rollback == nil || rollback == sql.ErrTxDone {
		return primary
	}
	return withDatabaseCleanupFailure(operation, primary, rollback)
}
