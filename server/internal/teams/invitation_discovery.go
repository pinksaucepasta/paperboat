package teams

import (
	"context"
	"database/sql"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type InvitationSummary struct {
	Invitation
	LastSentAt     *time.Time `json:"last_sent_at"`
	DeliveryState  string     `json:"delivery_state"`
	State          string     `json:"state"`
	CreatedBy      string     `json:"created_by"`
	DisplayName    string     `json:"display_name"`
	CreatedByEmail string     `json:"created_by_email"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (s *Service) InvitationInventory(ctx context.Context, account, team, q, state, owner, direction string, limit, offset int) ([]InvitationSummary, int, error) {
	if direction == "" {
		direction = "received"
	}
	if direction != "received" && direction != "sent" {
		return nil, 0, ErrInvalid
	}
	return s.invitationInventory(ctx, account, team, "", q, state, owner, direction, limit, offset)
}
func (s *Service) Invitation(ctx context.Context, account, id string) (InvitationSummary, error) {
	items, _, err := s.invitationInventory(ctx, account, "", id, "", "", "", "", 1, 0)
	if err != nil {
		return InvitationSummary{}, err
	}
	if len(items) == 0 {
		return InvitationSummary{}, ErrNotFound
	}
	return items[0], nil
}
func (s *Service) invitationInventory(ctx context.Context, account, team, id, q, state, owner, direction string, limit, offset int) ([]InvitationSummary, int, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, 0, ErrInvalid
	}
	predicate := ` FROM team_invitations i JOIN teams t USING(team_id) JOIN users creator ON creator.id=i.created_by LEFT JOIN LATERAL (SELECT u.primary_email,u.display_name FROM users u WHERE u.id=i.recipient_account OR (i.recipient_account IS NULL AND u.status='active' AND u.email_verified AND paperboat.canonical_email_address(u.primary_email)=i.recipient_email) ORDER BY u.id LIMIT 1) invitee ON true JOIN users viewer ON viewer.id=$1 AND viewer.status='active' WHERE t.deleted_at IS NULL
 AND (` + invitationRecipientSQL + ` OR EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=i.team_id AND m.account_id=$1 AND m.active AND team_role_allows(CASE WHEN t.owner_account=$1 THEN 'owner' ELSE m.role END,'members.manage')))
 AND ($2='' OR i.team_id=$2) AND ($3='' OR i.invitation_id=$3)
 AND ($4='' OR position(lower($4) in lower(i.team_id||' '||coalesce(i.recipient_account,'')||' '||coalesce(i.recipient_email,'')||' '||i.invitation_id))>0)
 AND ($5='' OR CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.cancelled_at IS NOT NULL THEN 'cancelled' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END=$5)
 AND ($6='' OR ($6='mine' AND ` + invitationRecipientSQL + `) OR ($6='shared' AND i.created_by=$1) OR i.recipient_account=$6 OR i.created_by=$6)`
	if team == "" && id == "" {
		if direction == "sent" {
			predicate += " AND i.created_by=$1"
		} else {
			predicate += " AND " + invitationRecipientSQL
		}
	}
	args := []any{account, team, id, q, state, owner}
	var total int
	if err := s.db.SQL().QueryRowContext(ctx, "SELECT count(*)"+predicate, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT i.invitation_id,i.team_id,coalesce(i.recipient_account,''),coalesce(i.recipient_email,invitee.primary_email,''),i.expires_at,t.generation,i.created_by,i.role,coalesce(invitee.display_name,''),creator.primary_email,i.created_at,(SELECT max(accepted_at) FROM transactional_mail_outbox WHERE invitation_id=i.invitation_id),coalesce((SELECT CASE WHEN state='cancelled' THEN 'failed' ELSE state END FROM transactional_mail_outbox WHERE mail_id=i.latest_mail_id),'unconfigured'),CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.cancelled_at IS NOT NULL THEN 'cancelled' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END`+predicate+` ORDER BY i.expires_at DESC,i.invitation_id DESC LIMIT $7 OFFSET $8`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	items := []InvitationSummary{}
	for rows.Next() {
		var v InvitationSummary
		if err = rows.Scan(&v.InvitationID, &v.TeamID, &v.AccountID, &v.Email, &v.ExpiresAt, &v.Generation, &v.CreatedBy, &v.Role, &v.DisplayName, &v.CreatedByEmail, &v.CreatedAt, &v.LastSentAt, &v.DeliveryState, &v.State); err != nil {
			return nil, 0, errors.Join(err, rows.Close())
		}
		items = append(items, v)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func validTeamName(name string) bool {
	if len(name) == 0 || len(name) > 128 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *Service) DeclineInvitation(ctx context.Context, account, id string, request AcceptRequest) (InvitationSummary, error) {
	var result Invitation
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := Lock(ctx, tx); err != nil {
			return err
		}
		done, digest, err := replay(ctx, tx, account, request.OperationID, "decline", []any{id, request}, &result)
		if err != nil {
			return err
		}
		var recipient bool
		var team string
		var accepted, cancelled sql.NullTime
		err = tx.QueryRow(ctx, `SELECT `+invitationRecipientSQL+`,i.team_id,i.accepted_at,i.cancelled_at FROM team_invitations i JOIN teams t USING(team_id) JOIN users viewer ON viewer.id=$2 AND viewer.status='active' WHERE i.invitation_id=$1 AND t.deleted_at IS NULL FOR UPDATE OF i`, id, account).Scan(&recipient, &team, &accepted, &cancelled)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !recipient {
			return ErrForbidden
		}
		if done {
			return nil
		}
		if accepted.Valid {
			return ErrConflict
		}
		if cancelled.Valid {
			return ErrConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE team_invitations SET recipient_account=$2,cancelled_at=now() WHERE invitation_id=$1`, id, account); err != nil {
			return err
		}
		if err = advance(ctx, tx, team); err != nil {
			return err
		}
		if err = AuditTx(ctx, tx, account, team, "invitation_declined", request.OperationID, nil); err != nil {
			return err
		}
		result = Invitation{InvitationID: id, TeamID: team, AccountID: account}
		return save(ctx, tx, account, request.OperationID, digest, result)
	})
	if err != nil {
		return InvitationSummary{}, err
	}
	return s.Invitation(ctx, account, id)
}
