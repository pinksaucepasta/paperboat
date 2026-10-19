package store

import (
	"context"
	"database/sql"
	"reflect"

	"modernc.org/sqlite"
)

func classify(err error) error {
	if err == nil {
		return nil
	}
	return err
}

func classifyDBError(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := sqliteFailureCode(err); ok && (code == 11 || code == 26) {
		return storeFailure("runtime state database is corrupt", ErrCorrupt, err)
	}
	return storeFailure("runtime state database failed", err)
}

// The store returns original causes without displaying SQL, paths, schema
// diffs, or terminal/environment values. Callers retain final report ownership.
type storeFailureError struct {
	message string
	causes  []error
}

func (e *storeFailureError) Error() string { return e.message }

func (e *storeFailureError) Unwrap() []error { return e.causes }

func storeFailure(message string, causes ...error) error {
	kept := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			kept = append(kept, cause)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return &storeFailureError{message: message, causes: kept}
}

func protectStoreError(err *error) {
	if *err == nil {
		return
	}
	switch *err {
	case ErrConflict, ErrNotFound, ErrReplayGap, ErrResultTooLarge, ErrIncompatible, ErrCorrupt, context.Canceled, context.DeadlineExceeded:
		return
	}
	if gap, ok := (*err).(*GapError); ok && gap != nil {
		return
	}
	if _, safe := (*err).(*storeFailureError); !safe {
		*err = storeFailure("runtime state operation failed", *err)
	}
}

func retainRollback(tx *sql.Tx, err *error) {
	if cleanup := tx.Rollback(); cleanup != nil && cleanup != sql.ErrTxDone {
		*err = storeFailure("runtime state transaction cleanup failed", *err, cleanup)
	}
}

// A mixed/unknown chain never authorizes either a busy retry or corruption
// classification. Package-owned single-cause wrappers preserve native authority.
func sqliteFailureCode(err error) (int, bool) {
	active := make(map[error]bool)
	for range 16 {
		if err == nil {
			return 0, false
		}
		value := reflect.ValueOf(err)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return 0, false
		}
		if value.Type().Comparable() {
			if active[err] {
				return 0, false
			}
			active[err] = true
		}
		if native, ok := err.(*sqlite.Error); ok {
			return native.Code() & 255, true
		}
		if own, ok := err.(*storeFailureError); ok {
			if len(own.causes) != 1 {
				return 0, false
			}
			err = own.causes[0]
			continue
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return 0, false
		}
		wrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return 0, false
		}
		err = wrapper.Unwrap()
	}
	return 0, false
}

func retryableSQLiteFailure(err error) bool {
	code, ok := sqliteFailureCode(err)
	return ok && (code == 5 || code == 6)
}
