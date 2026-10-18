// Package transactionalmail sends durable product mail without owning product state.
package transactionalmail

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
)

const (
	maxAttempts    = 8
	retryHorizon   = 23 * time.Hour
	requestTimeout = 15 * time.Second
	responseLimit  = 64 << 10
)

type Message struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

type Service struct {
	DB            *db.DB
	Endpoint, Key string
}

// EnqueueTx stores immutable provider bytes and a stable provider idempotency key
// atomically with the caller's product change. A nil invitation is generic mail.
func EnqueueTx(ctx context.Context, tx *db.Tx, id string, invitation *string, message Message) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(payload) > responseLimit || len(id) < 1 || len(id) > 256 {
		return errors.New("mail request exceeds payload or idempotency key bounds")
	}
	_, err = tx.Exec(ctx, `INSERT INTO transactional_mail_outbox(mail_id,invitation_id,payload) VALUES($1,$2,$3)`, id, invitation, payload)
	return err
}

// EnqueueOnceTx is EnqueueTx for notices that must be sent at most once per stable id:
// a repeated call with the same id is a no-op, and it reports whether it queued the mail.
func EnqueueOnceTx(ctx context.Context, tx *db.Tx, id string, message Message) (bool, error) {
	payload, err := json.Marshal(message)
	if err != nil {
		return false, err
	}
	if len(payload) > responseLimit || len(id) < 1 || len(id) > 256 {
		return false, errors.New("mail request exceeds payload or idempotency key bounds")
	}
	result, err := tx.Exec(ctx, `INSERT INTO transactional_mail_outbox(mail_id,payload) VALUES($1,$2) ON CONFLICT(mail_id) DO NOTHING`, id, payload)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

// Dispatch sends at most one item. Claim leases survive process crashes. Provider
// calls never run in a database transaction; retries retain both key and payload.
func (s *Service) Dispatch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.Key == "" {
		return nil
	}
	var id string
	var payload []byte
	var attempts int
	// Suppress obsolete invitation messages, including earlier resend generations.
	_, err := s.DB.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox o SET state='cancelled' WHERE state='queued' AND invitation_id IS NOT NULL AND EXISTS(SELECT 1 FROM team_invitations i JOIN teams t USING(team_id) WHERE i.invitation_id=o.invitation_id AND (i.accepted_at IS NOT NULL OR i.cancelled_at IS NOT NULL OR i.expires_at<=now() OR t.deleted_at IS NOT NULL OR t.suspended OR i.latest_mail_id IS DISTINCT FROM o.mail_id))`)
	if err != nil {
		return err
	}
	// Resend retains keys for 24h. Never retry ambiguous sends beyond 23h.
	_, err = s.DB.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox SET state='failed' WHERE state='queued' AND (attempts>=$1 OR first_attempt_at<now()-$2*interval '1 second') AND (lease_until IS NULL OR lease_until<now())`, maxAttempts, int64(retryHorizon/time.Second))
	if err != nil {
		return err
	}
	err = s.DB.SQL().QueryRowContext(ctx, `UPDATE transactional_mail_outbox SET lease_until=now()+interval '1 minute',first_attempt_at=coalesce(first_attempt_at,now()),attempts=attempts+1 WHERE mail_id=(SELECT mail_id FROM transactional_mail_outbox WHERE state='queued' AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<now()) ORDER BY next_attempt_at,mail_id FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING mail_id,payload,attempts`).Scan(&id, &payload, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	accepted, provider, retry, retryAfter := s.send(ctx, id, payload)
	if accepted {
		_, err = s.DB.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox SET state='sent',accepted_at=now(),provider_id=$2,lease_until=NULL,payload='{}' WHERE mail_id=$1`, id, provider)
	} else {
		state := "failed"
		if retry && attempts < maxAttempts {
			state = "queued"
		}
		delay := time.Duration(1<<uint(attempts)) * time.Minute
		if retryAfter > delay {
			delay = retryAfter
		}
		_, err = s.DB.SQL().ExecContext(ctx, `UPDATE transactional_mail_outbox SET state=$2,lease_until=NULL,next_attempt_at=now()+$3*interval '1 second' WHERE mail_id=$1`, id, state, int64(delay/time.Second))
	}
	return err
}

func (s *Service) send(ctx context.Context, id string, payload []byte) (bool, string, bool, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, "", false, 0
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", id)
	client := http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return false, "", true, 0
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil || len(body) > responseLimit {
		return false, "", true, 0
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var result struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &result) != nil || result.ID == "" {
			return false, "", true, 0
		}
		return true, result.ID, false, 0
	}
	var result struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &result)
	var retryAfter time.Duration
	if n, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); e == nil && n > 0 {
		if n > int64(retryHorizon/time.Second) {
			n = int64(retryHorizon / time.Second)
		}
		retryAfter = time.Duration(n) * time.Second
	} else if date, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
		retryAfter = time.Until(date)
		if retryAfter < 0 {
			retryAfter = 0
		}
		if retryAfter > retryHorizon {
			retryAfter = retryHorizon
		}
	}
	return false, "", resp.StatusCode == 429 || resp.StatusCode >= 500 || (resp.StatusCode == 409 && result.Name == "concurrent_idempotent_requests"), retryAfter
}

// Run keeps transient database failures recoverable and reports only safe worker
// metadata. Each iteration has its own deadline; shutdown interrupts the wait.
func (s *Service) Run(ctx context.Context) error {
	for {
		finish := workers.BeginIteration(ctx, time.Second)
		err := s.Dispatch(ctx)
		finish(err)
		delay := time.Second
		if err != nil {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
