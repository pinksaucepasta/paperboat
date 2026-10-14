package teams

import (
	"context"
	"database/sql"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"strings"
	"unicode"
	"unicode/utf8"
)

type InvitationSummary struct {
	Invitation
	State     string `json:"state"`
	CreatedBy string `json:"created_by"`
}

func (s *Service) InvitationInventory(ctx context.Context, account, team, q, state, owner string, limit, offset int) ([]InvitationSummary, int, error) {
	return s.invitationInventory(ctx, account, team, "", q, state, owner, limit, offset)
}
func (s *Service) Invitation(ctx context.Context, account, id string) (InvitationSummary, error) {
	items, _, err := s.invitationInventory(ctx, account, "", id, "", "", "", 1, 0)
	if err != nil {
		return InvitationSummary{}, err
	}
	if len(items) == 0 {
		return InvitationSummary{}, ErrNotFound
	}
	return items[0], nil
}
func (s *Service) invitationInventory(ctx context.Context, account, team, id, q, state, owner string, limit, offset int) ([]InvitationSummary, int, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, 0, ErrInvalid
	}
	predicate := ` FROM team_invitations i JOIN teams t USING(team_id) WHERE t.deleted_at IS NULL
 AND EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')
 AND (i.recipient_account=$1 OR EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=i.team_id AND m.account_id=$1 AND m.active AND (m.role='admin' OR t.owner_account=$1)))
 AND ($2='' OR i.team_id=$2) AND ($3='' OR i.invitation_id=$3)
 AND ($4='' OR position(lower($4) in lower(i.team_id||' '||i.recipient_account||' '||i.invitation_id))>0)
 AND ($5='' OR CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.cancelled_at IS NOT NULL THEN 'cancelled' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END=$5)
 AND ($6='' OR ($6='mine' AND i.recipient_account=$1) OR ($6='shared' AND i.created_by=$1) OR i.recipient_account=$6 OR i.created_by=$6)`
	args := []any{account, team, id, q, state, owner}
	var total int
	if err := s.db.SQL().QueryRowContext(ctx, "SELECT count(*)"+predicate, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT i.invitation_id,i.team_id,i.recipient_account,i.expires_at,t.generation,i.created_by,CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.cancelled_at IS NOT NULL THEN 'cancelled' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END`+predicate+` ORDER BY i.expires_at DESC,i.invitation_id DESC LIMIT $7 OFFSET $8`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	items := []InvitationSummary{}
	for rows.Next() {
		var v InvitationSummary
		if err = rows.Scan(&v.InvitationID, &v.TeamID, &v.AccountID, &v.ExpiresAt, &v.Generation, &v.CreatedBy, &v.State); err != nil {
			return nil, 0, errors.Join(err, rows.Close())
		}
		v.Role = "member"
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
		var recipient, team string
		var accepted, cancelled sql.NullTime
		err = tx.QueryRow(ctx, `SELECT i.recipient_account,i.team_id,i.accepted_at,i.cancelled_at FROM team_invitations i JOIN teams t USING(team_id) JOIN users u ON u.id=$2 AND u.status='active' WHERE i.invitation_id=$1 AND t.deleted_at IS NULL FOR UPDATE OF i`, id, account).Scan(&recipient, &team, &accepted, &cancelled)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if recipient != account {
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
		if _, err = tx.Exec(ctx, `UPDATE team_invitations SET cancelled_at=now() WHERE invitation_id=$1`, id); err != nil {
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
