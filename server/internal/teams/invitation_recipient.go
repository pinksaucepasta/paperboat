package teams

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// All recipient actions use the same current identity predicate. An email
// invitation binds permanently to an account when accepted or declined.
const invitationRecipientSQL = `(coalesce(i.recipient_account=viewer.id,false) OR
 (i.recipient_account IS NULL AND viewer.email_verified
 AND paperboat.canonical_email_address(i.recipient_email)=paperboat.canonical_email_address(viewer.primary_email)))`

func checkNewInvitation(ctx context.Context, tx *db.Tx, team string, r InviteRequest) error {
	var err error
	var email string
	if r.AccountID != "" {
		if err = tx.QueryRow(ctx, `SELECT CASE WHEN email_verified THEN paperboat.canonical_email_address(primary_email) ELSE '' END FROM users WHERE id=$1 AND status='active'`, r.AccountID).Scan(&email); err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	} else {
		email = r.Email
		var existingMember bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_members m JOIN users u ON u.id=m.account_id
 WHERE m.team_id=$1 AND m.active AND u.status='active'
 AND u.email_verified AND paperboat.canonical_email_address(u.primary_email)=$2)`, team, email).Scan(&existingMember); err != nil {
			return err
		}
		if existingMember {
			return ErrConflict
		}
	}
	var verifiedDomain bool
	if err = tx.QueryRow(ctx, `SELECT NOT t.sso_required OR EXISTS(SELECT 1 FROM team_sso_domains d WHERE d.team_id=t.team_id AND d.domain=lower(split_part($2,'@',2)) AND $2<>'') FROM teams t WHERE t.team_id=$1`, team, email).Scan(&verifiedDomain); err != nil {
		return err
	}
	if !verifiedDomain {
		return ErrDomainNotVerified
	}
	if _, err = tx.Exec(ctx, `UPDATE team_invitations SET cancelled_at=now() WHERE team_id=$1 AND expires_at<=now() AND accepted_at IS NULL AND cancelled_at IS NULL`, team); err != nil {
		return err
	}
	var count int
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT count(*),coalesce(bool_or(
 recipient_account=nullif($2,'') OR ($3<>'' AND
  (paperboat.canonical_email_address(recipient_email)=$3 OR recipient_account IN
   (SELECT id FROM users WHERE status='active' AND email_verified AND paperboat.canonical_email_address(primary_email)=$3)))
 ),false) FROM team_invitations
 WHERE team_id=$1 AND accepted_at IS NULL AND cancelled_at IS NULL`, team, r.AccountID, email).Scan(&count, &exists); err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	if count >= MaximumInvitations {
		return ErrLimit
	}
	if err = capacity(ctx, tx, r.AccountID, team); err != nil {
		return err
	}
	return nil
}
