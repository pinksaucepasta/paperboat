// Package emailpolicy owns the signup and invitation email admission contract.
package emailpolicy

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var (
	ErrInvalid = errors.New("Enter a valid email address.")
)

// Normalize shares the database identity function with account lookup and
// invitations: lowercase and remove the first plus suffix, preserving dots.
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
	if strings.SplitN(local, "+", 2)[0] == "" {
		return "", ErrInvalid
	}
	var canonical string
	err = tx.QueryRow(ctx, `SELECT paperboat.canonical_email_address($1)`, value).Scan(&canonical)
	return canonical, err
}
