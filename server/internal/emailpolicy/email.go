// Package emailpolicy owns the signup and invitation email admission contract.
package emailpolicy

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var (
	ErrInvalid     = errors.New("Enter a valid email address.")
	ErrPlusAddress = errors.New("Email addresses containing + cannot be used. Sign in with your original email address.")
	ErrDuplicate   = errors.New("An account already uses this email address. Sign in to the existing account.")
)

// Normalize uses the database's canonical identity function, also used when
// matching legacy accounts and invitations. Legacy account rows are not rewritten.
func Normalize(ctx context.Context, tx *db.Tx, value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(value)
	if len(value) > 254 || err != nil || parsed.Name != "" || parsed.Address != value {
		return "", ErrInvalid
	}
	local, _, ok := strings.Cut(value, "@")
	if !ok {
		return "", ErrInvalid
	}
	if strings.Contains(local, "+") {
		return "", ErrPlusAddress
	}
	var canonical string
	err = tx.QueryRow(ctx, `SELECT paperboat.canonical_email_address($1)`, value).Scan(&canonical)
	return canonical, err
}
