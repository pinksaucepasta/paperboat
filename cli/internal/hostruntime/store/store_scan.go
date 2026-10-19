package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store/storesqlc"
)

type scanner interface{ Scan(...any) error }

func operationFromSQLC(operationID string, requestHash []byte, state string, result []byte, errorCode string, completed sql.NullInt64, expires int64) OperationResult {
	operation := OperationResult{OperationID: operationID, RequestHash: requestHash, State: state, Result: result, ErrorCode: errorCode}
	if completed.Valid {
		operation.CompletedAt = time.Unix(0, completed.Int64).UTC()
	}
	operation.ExpiresAt = time.Unix(0, expires).UTC()
	return operation
}

func sessionFromSQLC(row storesqlc.ListSessionsRow) (Session, error) {
	session := Session{
		ID: row.ID, Name: row.Name, CWD: row.Cwd, CommandPath: row.CommandPath,
		Columns: uint16(row.Columns), Rows: uint16(row.Rows), State: row.State,
		Generation: uint64(row.Generation), ExitSignal: row.ExitSignal,
		EarliestSequence: uint64(row.EarliestSequence), LatestSequence: uint64(row.LatestSequence),
		CreatedAt: time.Unix(0, row.CreatedAt).UTC(), UpdatedAt: time.Unix(0, row.UpdatedAt).UTC(),
	}
	if err := json.Unmarshal(row.CommandArgs, &session.CommandArgs); err != nil {
		return Session{}, storeFailure("runtime session arguments are invalid", ErrCorrupt, err)
	}
	if err := json.Unmarshal(row.CommandEnv, &session.CommandEnv); err != nil {
		return Session{}, storeFailure("runtime session environment is invalid", ErrCorrupt, err)
	}
	if row.ExitCode.Valid {
		value := int(row.ExitCode.Int64)
		session.ExitCode = &value
	}
	if row.ExitedAt.Valid {
		value := time.Unix(0, row.ExitedAt.Int64).UTC()
		session.ExitedAt = &value
	}
	return session, nil
}

func nullableInt(value *int) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*value), Valid: true}
}

func nullableSQLString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullableTimestamp(value *time.Time) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value.UnixNano(), Valid: true}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixNano()
}
