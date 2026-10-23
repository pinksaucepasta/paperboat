package teams

import (
	"context"
	"errors"
	"html"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// ResendCooldown bounds provider work for a single invitation, including failed sends.
const ResendCooldown = time.Minute

type RetryAfterError struct{ After time.Duration }

func (e *RetryAfterError) Error() string {
	return "invitation email requested recently; retry after the cooldown"
}

func (s *Service) enqueueInvitation(ctx context.Context, tx *db.Tx, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE team_invitations SET latest_mail_id=NULL,mail_requested_at=now() WHERE invitation_id=$1`, id); err != nil {
		return err
	}
	// Keep only the latest confirmed acceptance and any live lease; obsolete
	// requests must not grow an invitation's private mail history indefinitely.
	if _, err := tx.Exec(ctx, `DELETE FROM transactional_mail_outbox o WHERE invitation_id=$1 AND (lease_until IS NULL OR lease_until<now()) AND mail_id IS DISTINCT FROM (SELECT mail_id FROM transactional_mail_outbox WHERE invitation_id=$1 AND accepted_at IS NOT NULL ORDER BY accepted_at DESC,mail_id DESC LIMIT 1)`, id); err != nil {
		return err
	}
	if !s.mailEnabled {
		return nil
	}
	var address, name string
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT coalesce(i.recipient_email,CASE WHEN u.email_verified AND u.status='active' THEN u.primary_email END,''),coalesce(t.display_name,t.team_id),i.expires_at FROM team_invitations i JOIN teams t USING(team_id) LEFT JOIN users u ON u.id=i.recipient_account WHERE i.invitation_id=$1`, id).Scan(&address, &name, &expires)
	if err != nil {
		return err
	}
	if address == "" {
		return nil
	}
	mailID := "mail_" + uuid.NewString()
	message := transactionalmail.Message{From: s.mail.From, To: []string{address}, Subject: "Paperboat team invitation", HTML: "<p>You are invited to join " + html.EscapeString(name) + " on Paperboat.</p><p>This invitation expires " + expires.UTC().Format(time.RFC3339) + ".</p><p><a href=\"" + html.EscapeString(strings.TrimRight(s.mail.ConsoleOrigin, "/")+"/team") + "\">Sign in to view your team invitation</a></p>"}
	if err = transactionalmail.EnqueueTx(ctx, tx, mailID, &id, message); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE team_invitations SET latest_mail_id=$2,mail_requested_at=now() WHERE invitation_id=$1`, id, mailID)
	return err
}

func (s *Service) ResendInvitation(ctx context.Context, account, team, id string, r AcceptRequest) (InvitationSummary, error) {
	var receipt Invitation
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := Lock(ctx, tx); err != nil {
			return err
		}
		done, digest, err := replay(ctx, tx, account, r.OperationID, "resend_invitation", []any{team, id, r}, &receipt)
		if err != nil {
			return err
		}
		t, err := ReadTx(ctx, tx, team)
		if err != nil {
			return err
		}
		m, ok := currentMember(t, account)
		if !ok || t.Deleted || !Allowed(m.Role, PermissionMembersManage) {
			return ErrForbidden
		}
		if t.Suspended {
			return ErrSuspended
		}
		var usable, expired bool
		var role string
		var wait float64
		err = tx.QueryRow(ctx, `SELECT role,accepted_at IS NULL AND cancelled_at IS NULL,expires_at<=now(),greatest(0,extract(epoch FROM coalesce(mail_requested_at,'-infinity'::timestamptz)+$3*interval '1 second'-now())) FROM team_invitations WHERE invitation_id=$1 AND team_id=$2 FOR UPDATE`, id, team, int64(ResendCooldown/time.Second)).Scan(&role, &usable, &expired, &wait)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !mayGrantRole(m.Role, role) {
			return ErrForbidden
		}
		if !usable {
			return ErrExpired
		}
		if done {
			return nil
		}
		if wait > 0 {
			return &RetryAfterError{After: time.Duration(wait * float64(time.Second))}
		}
		// Expired invitations consume no seat. Capacity is checked before revival.
		if expired {
			if err = reserveInvitationSeatTx(ctx, tx, team); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE team_invitations SET expires_at=now()+$2*interval '1 second' WHERE invitation_id=$1`, id, s.mail.InvitationLifetimeSeconds)
		if err != nil {
			return err
		}
		if err = s.enqueueInvitation(ctx, tx, id); err != nil {
			return err
		}
		if err = advance(ctx, tx, team); err != nil {
			return err
		}
		if err = AuditTx(ctx, tx, account, team, "invitation_resent", r.OperationID, nil); err != nil {
			return err
		}
		receipt = Invitation{InvitationID: id, TeamID: team}
		return save(ctx, tx, account, r.OperationID, digest, receipt)
	})
	if err != nil {
		return InvitationSummary{}, err
	}
	return s.Invitation(ctx, account, id)
}
