// Package customernotices is how an operator sends the notices the Terms, Subprocessors page and DPA promise to
// customers by email: a price increase, a terms change, a new subprocessor, and a breach notice. The operator previews
// (dry run: the rendered mail and the recipient count), then sends; each notice is one audited row, and its mails go
// through the transactional outbox, one per recipient under a stable id, so a retried request never mails twice.
package customernotices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/announcements"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

var (
	ErrInvalid     = errors.New("invalid notice")
	ErrConflict    = errors.New("notice conflict")
	ErrMailOff     = errors.New("transactional mail is not configured")
	ErrTooManyMail = errors.New("too many recipients")
)

const (
	// MaxRecipients bounds one notice; larger audiences are split by plan or team.
	MaxRecipients = 20000
	batch         = 500

	audienceAll  = "all"
	audiencePaid = "paid"
	paidStates   = `b.state IN ('active','past_due')`
	eligible     = `u.status='active' AND u.email_verified AND u.primary_email<>'' AND u.role<>'system_worker'`
	// recipients: the account holders to write to. $1 is the keyset cursor, $2 the audience value. A paid workspace's
	// contact is its personal owner or its team owner and admins; a team audience is the team's owner and admins.
	recipients = eligible + ` AND u.id>$1 AND (
 $3='all'
 OR ($3='user' AND u.id=$2)
 OR ($3='team' AND u.id IN (SELECT m.account_id FROM paperboat.team_members m JOIN paperboat.teams t ON t.team_id=m.team_id AND t.deleted_at IS NULL
    WHERE m.team_id=$2 AND m.active AND (t.owner_account=m.account_id OR m.role='admin')))
 OR ($3='plan' AND $2='free' AND NOT EXISTS(SELECT 1 FROM paperboat.billing_workspaces b WHERE b.user_id=u.id AND b.product_code IS NOT NULL AND ` + paidStates + `))
 OR (($3='paid' OR ($3='plan' AND $2<>'free')) AND u.id IN (
    SELECT b.user_id FROM paperboat.billing_workspaces b WHERE b.user_id IS NOT NULL AND b.product_code IS NOT NULL AND ` + paidStates + ` AND ($3='paid' OR b.product_code=$2)
    UNION SELECT m.account_id FROM paperboat.billing_workspaces b JOIN paperboat.teams t ON t.team_id=b.team_id AND t.deleted_at IS NULL
      JOIN paperboat.team_members m ON m.team_id=t.team_id AND m.active AND (t.owner_account=m.account_id OR m.role='admin')
      WHERE b.product_code IS NOT NULL AND ` + paidStates + ` AND ($3='paid' OR b.product_code=$2))))`
)

type Service struct {
	store       *db.DB
	audit       *audit.Writer
	mail        config.TransactionalMail
	mailEnabled bool
}

func New(store *db.DB) *Service { return &Service{store: store, audit: audit.NewWriter(store)} }

// ConfigureMail supplies the deployment's mail policy; without it nothing can be sent.
func (s *Service) ConfigureMail(policy config.TransactionalMail, enabled bool) {
	s.mail, s.mailEnabled = policy, enabled
}

// Notice is one notice as the operator sees it: previewed (no ID) or sent.
type Notice struct {
	ID            string     `json:"id,omitempty"`
	Kind          string     `json:"kind"`
	AudienceKind  string     `json:"audience_kind"`
	AudienceValue string     `json:"audience_value,omitempty"`
	Subject       string     `json:"subject"`
	HTML          string     `json:"html"`
	Recipients    int        `json:"recipients"`
	Late          bool       `json:"late,omitempty"`
	Deadline      *time.Time `json:"deadline,omitempty"`
	CreatedBy     string     `json:"created_by,omitempty"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	QueuedAt      *time.Time `json:"queued_at,omitempty"`
}

func (s *Service) count(ctx context.Context, tx *db.Tx, in Input) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM paperboat.users u WHERE `+recipients, "", in.AudienceValue, in.AudienceKind).Scan(&n)
	return n, err
}

// Send previews (DryRun) or sends a notice. A retried key returns the notice it created and finishes queueing its mail.
func (s *Service) Send(ctx context.Context, actor, key string, in Input) (Notice, error) {
	if len(key) < 8 || len(key) > 256 {
		return Notice{}, fmt.Errorf("%w: an Idempotency-Key of 8 to 256 characters is required", ErrInvalid)
	}
	switch in.AudienceKind {
	case audienceAll, audiencePaid:
		in.AudienceValue = ""
	case announcements.AudiencePlan, announcements.AudienceTeam, announcements.AudienceUser:
		if in.AudienceValue == "" || len(in.AudienceValue) > 128 {
			return Notice{}, fmt.Errorf("%w: the audience needs a value", ErrInvalid)
		}
	default:
		return Notice{}, fmt.Errorf("%w: audience is all, paid, plan, team or user", ErrInvalid)
	}
	now := time.Now()
	subject, body, late, deadline, err := render(in, now)
	if err != nil {
		return Notice{}, err
	}
	out := Notice{Kind: in.Kind, AudienceKind: in.AudienceKind, AudienceValue: in.AudienceValue, Subject: subject, HTML: body, Late: late}
	if !deadline.IsZero() {
		out.Deadline = &deadline
	}
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := announcements.AudienceExists(ctx, tx, in.AudienceKind, in.AudienceValue); err != nil {
			return err // announcements.ErrInvalid: no such team, user or plan
		}
		n, err := s.count(ctx, tx, in)
		out.Recipients = n
		return err
	})
	if err != nil || in.DryRun {
		return out, err
	}
	if !s.mailEnabled {
		return Notice{}, ErrMailOff
	}
	if out.Recipients > MaxRecipients {
		return Notice{}, fmt.Errorf("%w: %d recipients; send to a plan or a team instead (at most %d)", ErrTooManyMail, out.Recipients, MaxRecipients)
	}
	request, _ := json.Marshal(in)
	sum := sha256.Sum256(request)
	hash := hex.EncodeToString(sum[:])
	id := "notice_" + uuid.NewString()
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		// One statement decides the race between two requests with the same key: the loser reads the winner's row.
		inserted, err := tx.Exec(ctx, `INSERT INTO paperboat.customer_notices(id,kind,audience_kind,audience_value,subject,html,request_hash,late,created_by,idempotency_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (created_by,idempotency_key) DO NOTHING`,
			id, in.Kind, in.AudienceKind, in.AudienceValue, subject, body, hash, late, actor, key)
		if err != nil {
			return err
		}
		if inserted.RowsAffected() == 0 {
			var oldHash string
			if err = tx.QueryRow(ctx, `SELECT id,request_hash FROM paperboat.customer_notices WHERE created_by=$1 AND idempotency_key=$2`, actor, key).Scan(&id, &oldHash); err != nil {
				return err
			}
			if oldHash != hash {
				return fmt.Errorf("%w: this Idempotency-Key was used for a different notice", ErrConflict)
			}
			return nil
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "operator.customer_notice_send", ResourceType: "customer_notice", ResourceID: id, IdempotencyKey: key,
			Metadata: map[string]any{"kind": in.Kind, "audience_kind": in.AudienceKind, "audience_value": in.AudienceValue, "recipients": out.Recipients, "late": late}})
	})
	if err != nil {
		return Notice{}, err
	}
	sent, err := s.queue(ctx, id, in, subject, body)
	if err != nil {
		return Notice{}, err
	}
	out.ID, out.Recipients = id, sent
	return out, nil
}

// queue writes one outbox mail per recipient in batches under a stable id, then records the count: a retry redoes it harmlessly.
func (s *Service) queue(ctx context.Context, id string, in Input, subject, body string) (int, error) {
	total, cursor := 0, ""
	for {
		n := 0
		err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			rows, err := tx.Query(ctx, `SELECT u.id,u.primary_email FROM paperboat.users u WHERE `+recipients+` ORDER BY u.id LIMIT `+fmt.Sprint(batch), cursor, in.AudienceValue, in.AudienceKind)
			if err != nil {
				return err
			}
			type to struct{ id, address string }
			var list []to
			for rows.Next() {
				var t to
				if err := rows.Scan(&t.id, &t.address); err != nil {
					rows.Close()
					return err
				}
				list = append(list, t)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, t := range list {
				if _, err := transactionalmail.EnqueueOnceTx(ctx, tx, "notice:"+id+":"+t.id, transactionalmail.Message{From: s.mail.From, To: []string{t.address}, Subject: subject, HTML: body}); err != nil {
					return err
				}
				cursor = t.id
			}
			n = len(list)
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("queue notice mail: %w", err)
		}
		total += n
		if n < batch {
			break
		}
	}
	_, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.customer_notices SET recipients=$2,queued_at=coalesce(queued_at,now()) WHERE id=$1`, id, total)
	return total, err
}

// List pages sent notices newest first.
func (s *Service) List(ctx context.Context, after int64, limit int) ([]Notice, string, error) {
	rows, err := s.store.Pool().Query(ctx, `SELECT id,kind,audience_kind,audience_value,subject,html,recipients,late,created_by,created_at,queued_at,seq FROM paperboat.customer_notices WHERE ($1=0 OR seq<$1) ORDER BY seq DESC LIMIT $2`, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out, next, last := []Notice{}, "", int64(0)
	for rows.Next() {
		var n Notice
		var seq int64
		if err := rows.Scan(&n.ID, &n.Kind, &n.AudienceKind, &n.AudienceValue, &n.Subject, &n.HTML, &n.Recipients, &n.Late, &n.CreatedBy, &n.CreatedAt, &n.QueuedAt, &seq); err != nil {
			return nil, "", err
		}
		if len(out) == limit {
			next = fmt.Sprint(last)
			break
		}
		out, last = append(out, n), seq
	}
	return out, next, rows.Err()
}
