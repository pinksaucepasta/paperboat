package auth

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/ipblocks"
)

var (
	// ErrAccountBanned: an operator banned this identity (its WorkOS subject, its e-mail without case or +suffix, or the
	// account the sign-in resolves to), so neither sign-in nor a new sign-up with it is allowed.
	ErrAccountBanned = errors.New("account is banned")
	// ErrIPBlocked: the sign-in comes from a network an operator blocked.
	ErrIPBlocked = errors.New("network is blocked")
)

// mapIPBlocked turns the block list's refusal into the browser sign-in's error; other errors pass.
func mapIPBlocked(err error) error {
	if errors.Is(err, ipblocks.ErrBlocked) {
		return ErrIPBlocked
	}
	return err
}

// checkNotBanned refuses a sign-in whose account, canonical e-mail or WorkOS subject is under a running ban, before any
// account or session exists for it. It runs after the identity is resolved, so a brand-new sign-up with a banned
// subject or e-mail (any case, any +suffix) is refused as well.
func checkNotBanned(ctx context.Context, tx *db.Tx, accountID, canonicalEmail, workOSSubject string) error {
	var banned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bans WHERE lifted_at IS NULL AND
 ((subject_kind='user' AND subject_id=$1) OR canonical_email=$2 OR workos_subject=$3))`, accountID, canonicalEmail, workOSSubject).Scan(&banned); err != nil {
		return err
	}
	if banned {
		return ErrAccountBanned
	}
	return nil
}
