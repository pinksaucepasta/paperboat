package operator

import (
	"context"
	"database/sql"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// MailFailure is a transactional mail that stayed undelivered past its retries (8 attempts or 23 h). The message body
// and recipient are never shown.
type MailFailure struct {
	ID             string     `json:"id"`
	InvitationID   string     `json:"invitation_id,omitempty"`
	Attempts       int32      `json:"attempts"`
	FirstAttemptAt *time.Time `json:"first_attempt_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

type MailFailurePage struct {
	Counted
	Mails     []MailFailure `json:"mails"`
	NextAfter string        `json:"next_after,omitempty"`
}

func readMailFailures(ctx context.Context, tx *db.Tx, target, after string, limit int) (_ MailFailurePage, resultErr error) {
	if target != "" {
		return MailFailurePage{}, ErrInvalid
	}
	total, err := countRows(ctx, tx, `transactional_mail_outbox WHERE state='failed'`)
	if err != nil {
		return MailFailurePage{}, err
	}
	rows, err := tx.Query(ctx, `SELECT mail_id,coalesce(invitation_id,''),attempts,first_attempt_at,created_at FROM transactional_mail_outbox WHERE state='failed' AND mail_id>$1 ORDER BY mail_id LIMIT $2`, after, limit+1)
	if err != nil {
		return MailFailurePage{}, err
	}
	defer func() { resultErr = closeRowsError(rows, resultErr) }()
	p := MailFailurePage{Counted: total, Mails: make([]MailFailure, 0, limit)}
	for rows.Next() {
		var m MailFailure
		var first sql.NullTime
		if err := rows.Scan(&m.ID, &m.InvitationID, &m.Attempts, &first, &m.CreatedAt); err != nil {
			return p, err
		}
		if first.Valid {
			m.FirstAttemptAt = &first.Time
		}
		if len(p.Mails) == limit {
			p.NextAfter = p.Mails[len(p.Mails)-1].ID
			break
		}
		p.Mails = append(p.Mails, m)
	}
	return p, rows.Err()
}

// RedriveMail puts a failed mail back in the queue with fresh attempts. The attempt clock restarts too: the 23 h
// horizon counts from first_attempt_at, so keeping it would fail the mail again at the next dispatch.
// ErrNotFound: no such mail, or it is not failed (sent mail must never be sent twice).
func (s *Service) RedriveMail(ctx context.Context, mailID string) error {
	tag, err := s.store.Pool().Exec(ctx, `UPDATE paperboat.transactional_mail_outbox SET state='queued',attempts=0,first_attempt_at=NULL,lease_until=NULL,next_attempt_at=now() WHERE mail_id=$1 AND state='failed'`, mailID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
