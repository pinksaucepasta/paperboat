package operatorauth

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

type operatorAuthCycleError struct{}

func (operatorAuthCycleError) Error() string   { return "cycle" }
func (e operatorAuthCycleError) Unwrap() error { return e }

type operatorAuthUncomparableError []error

func (operatorAuthUncomparableError) Error() string { return "uncomparable" }

func TestOperatorAuthNoRowsRequiresCompletePureCause(t *testing.T) {
	if !operatorAuthNoRowsOnly(fmt.Errorf("wrapped: %w", sql.ErrNoRows)) {
		t.Fatal("pure wrapped no-row result was not recognized")
	}
	for _, err := range []error{
		errors.Join(sql.ErrNoRows, syscall.EIO),
		errors.Join(sql.ErrNoRows, errors.New("unknown failure")),
		operatorAuthCycleError{},
		operatorAuthUncomparableError{sql.ErrNoRows},
	} {
		if operatorAuthNoRowsOnly(err) {
			t.Fatalf("mixed or incomplete cause tree was accepted as invalid credentials: %v", err)
		}
	}
	var typedNil *operatorAuthCycleError
	if operatorAuthNoRowsOnly(typedNil) {
		t.Fatal("typed nil was accepted as invalid credentials")
	}
}

func TestPasswordHash(t *testing.T) {
	encoded, err := passwordHash("a sufficiently long operator password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "argon2id$") || strings.Contains(encoded, "sufficiently") {
		t.Fatal("password was not protected")
	}
	if !verifyPassword(encoded, "a sufficiently long operator password") {
		t.Fatal("correct password rejected")
	}
	if verifyPassword(encoded, "a different operator password") {
		t.Fatal("wrong password accepted")
	}
	if _, err = passwordHash("short"); err == nil {
		t.Fatal("short password accepted")
	}
}
