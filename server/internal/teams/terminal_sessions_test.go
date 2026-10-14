package teams

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestTerminalSessionNoRowsDoesNotMaskRollbackFailure(t *testing.T) {
	for _, err := range []error{sql.ErrNoRows, pgx.ErrNoRows} {
		if !terminalSessionNoRows(err) {
			t.Errorf("direct no-row sentinel %T was not recognized", err)
		}
	}
	if terminalSessionNoRows(errors.Join(pgx.ErrNoRows, errors.New("rollback failed"))) {
		t.Fatal("joined rollback failure was classified as an expected missing session")
	}
}
