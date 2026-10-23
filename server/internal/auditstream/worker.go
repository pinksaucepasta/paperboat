package auditstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/transactionalmail"
)

// settle leaves the newest events for the next tick, so a late-committing event with a lower
// sequence is never passed by the cursor.
const settle = 30 * time.Second

// claimLease holds a due destination for one worker: longer than a tick's 2-minute bound, so
// another server process never sends the same batch at the same time.
const claimLease = 3 * time.Minute

// Deliver is one worker tick: every destination that is due gets the events after its cursor, in
// order, in batches of at most business.audit_stream_batch_bytes. Success moves the cursor;
// failure retries with exponential backoff up to business.audit_stream_max_backoff and, after
// business.audit_stream_alert_after of failures, emails the team's owners and admins once.
func (s *Service) Deliver(ctx context.Context) error {
	rows, err := s.db.SQL().QueryContext(ctx, `UPDATE paperboat.audit_stream_destinations d SET next_attempt_at=now()+make_interval(secs=>$1)
 FROM (SELECT team_id,kind FROM paperboat.audit_stream_destinations WHERE next_attempt_at<=now() ORDER BY next_attempt_at LIMIT 20 FOR UPDATE SKIP LOCKED) due
 WHERE d.team_id=due.team_id AND d.kind=due.kind RETURNING d.team_id,d.kind`, claimLease.Seconds())
	if err != nil {
		return err
	}
	type due struct{ team, kind string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.team, &d.kind); err == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	var errs []error
	for _, d := range list {
		if err := s.deliverOne(ctx, d.team, d.kind); err != nil {
			errs = append(errs, fmt.Errorf("stream %s/%s: %w", d.team, d.kind, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) deliverOne(ctx context.Context, team, kind string) error {
	var d Destination
	var sec Secrets
	if err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		d, sec, err = s.loadTx(ctx, tx, team, kind)
		return err
	}); err != nil {
		return err
	}
	events, err := s.log.Since(ctx, "team:"+team, d.cursor, 1000, settle)
	if err != nil || len(events) == 0 {
		// Nothing to send: release the claim so new events go out on the next tick.
		_, release := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.audit_stream_destinations SET next_attempt_at=now() WHERE team_id=$1 AND kind=$2 AND attempts=0`, team, kind)
		return errors.Join(err, release)
	}
	var body []byte
	var first, last int64
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if len(body) > 0 && len(body)+len(line)+1 > s.cfg.AuditStreamBatchBytes {
			break
		}
		if first == 0 {
			first = e.Sequence()
		}
		body = append(append(body, line...), '\n')
		last = e.Sequence()
	}
	now := s.now()
	sendErr := s.send(ctx, d, sec, body, first, last, now)
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if sendErr == nil {
			_, err := tx.Exec(ctx, `UPDATE audit_stream_destinations SET cursor_sequence=$3,last_delivered_at=now(),attempts=0,last_error=NULL,failing_since=NULL,alerted_at=NULL,next_attempt_at=now()
 WHERE team_id=$1 AND kind=$2 AND cursor_sequence=$4`, team, kind, last, d.cursor)
			return err
		}
		backoff := 10 * time.Second << min(d.attempts, 16)
		if backoff > s.cfg.AuditStreamMaxBackoff || backoff <= 0 {
			backoff = s.cfg.AuditStreamMaxBackoff
		}
		message := sendErr.Error()
		var delivery *DeliveryError
		if !errors.As(sendErr, &delivery) {
			message = "delivery failed"
		}
		var failingSince time.Time
		var alerted bool
		if err := tx.QueryRow(ctx, `UPDATE audit_stream_destinations SET attempts=attempts+1,last_error=$3,failing_since=coalesce(failing_since,now()),next_attempt_at=now()+make_interval(secs=>$4)
 WHERE team_id=$1 AND kind=$2 RETURNING failing_since,alerted_at IS NOT NULL`, team, kind, message, backoff.Seconds()).Scan(&failingSince, &alerted); err != nil {
			return err
		}
		if alerted || now.Sub(failingSince) < s.cfg.AuditStreamAlertAfter {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_stream_destinations SET alerted_at=now() WHERE team_id=$1 AND kind=$2`, team, kind); err != nil {
			return err
		}
		if err := record(ctx, tx, team, "activity_stream_failing", map[string]any{"resource_kind": kind, "reason": message}); err != nil {
			return err
		}
		return s.alertTx(ctx, tx, team, kind, message, failingSince)
	})
}

// alertTx emails the team's owners and admins once per failure episode.
func (s *Service) alertTx(ctx context.Context, tx *db.Tx, team, kind, message string, since time.Time) error {
	if !s.mailEnabled {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT u.id,u.primary_email,coalesce(t.display_name,t.team_id) FROM teams t JOIN team_members m ON m.team_id=t.team_id AND m.active
 JOIN users u ON u.id=m.account_id WHERE t.team_id=$1 AND team_role_allows(CASE WHEN t.owner_account=m.account_id THEN 'owner' ELSE m.role END,'audit.stream')
 AND u.status='active' AND u.email_verified AND u.primary_email<>'' ORDER BY u.id LIMIT 64`, team)
	if err != nil {
		return err
	}
	type recipient struct{ id, address, name string }
	var list []recipient
	for rows.Next() {
		var r recipient
		if err := rows.Scan(&r.id, &r.address, &r.name); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	link := strings.TrimRight(s.mail.ConsoleOrigin, "/") + "/team?team=" + team + "&tab=activity"
	label := map[string]string{"https": "HTTPS endpoint", "s3": "S3 storage"}[kind]
	for _, r := range list {
		msg := transactionalmail.Message{From: s.mail.From, To: []string{r.address}, Subject: "Paperboat: audit log streaming is failing",
			HTML: fmt.Sprintf("<p>Paperboat has not been able to deliver the audit log of %s to its %s since %s (UTC): %s.</p><p>Events are kept and delivered in order once the destination accepts them again.</p><p><a href=\"%s\">Check the destination</a></p>",
				html.EscapeString(r.name), label, since.UTC().Format("2 Jan 2006 15:04"), html.EscapeString(message), html.EscapeString(link))}
		if _, err := transactionalmail.EnqueueOnceTx(ctx, tx, "audit-stream-failing:"+team+":"+kind+":"+since.UTC().Format(time.RFC3339)+":"+r.id, msg); err != nil {
			return err
		}
	}
	return nil
}
