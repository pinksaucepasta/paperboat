package previewdomain

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapDatabaseErrorUsesConstraintIdentityNotMessageText(t *testing.T) {
	known := &pgconn.PgError{Code: "23505", ConstraintName: "domain_bindings_live_hostname_unique", Message: "custom domain hostname is already claimed"}
	if !errors.Is(mapDatabaseError(known), ErrDomainConflict) {
		t.Fatal("known hostname constraint was not classified as a domain conflict")
	}
	messageOnly := &pgconn.PgError{Code: "23505", ConstraintName: "other_unique_constraint", Message: "custom domain hostname is already claimed"}
	if errors.Is(mapDatabaseError(messageOnly), ErrDomainConflict) {
		t.Fatal("error message text selected a domain conflict classification")
	}
}
