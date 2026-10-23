package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

const (
	// maxJobAttempts bounds retries. A job that exhausts them stays failed, visible in the table.
	maxJobAttempts = 12
	// maxJobBackoff caps the delay between attempts.
	maxJobBackoff = 6 * time.Hour
	// jobLease is how long a claimed job is held before another worker may take it over.
	jobLease = 2 * time.Minute
	// jobTimeout bounds one provider call.
	jobTimeout = 30 * time.Second
)

// WorkOSUsers deletes a WorkOS user by subject. A user that no longer exists counts as deleted.
type WorkOSUsers interface {
	DeleteWorkOSUser(ctx context.Context, subject string) error
}

// Subscriptions cancels a Stripe subscription now. A subscription that no longer exists counts as cancelled.
type Subscriptions interface {
	CancelSubscription(ctx context.Context, subscriptionID string) error
}

// Outbox runs the provider work queued by account deletion. Every job is retried with backoff and
// bounded; a failure never reaches the deletion, which committed before the job ran.
type Outbox struct {
	db     *db.DB
	from   string
	workos WorkOSUsers
	stripe Subscriptions
}

// Worker returns the outbox for the given provider clients. A nil client fails its jobs until configured.
func (s *Service) Worker(workos WorkOSUsers, subscriptions Subscriptions) *Outbox {
	return &Outbox{db: s.db, from: s.mail.From, workos: workos, stripe: subscriptions}
}

// Run keeps dispatching jobs until ctx ends. Database errors are reported; job failures are recorded on the job.
func (o *Outbox) Run(ctx context.Context) error {
	for {
		finish := workers.BeginIteration(ctx, time.Second)
		err := o.Dispatch(ctx)
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

type job struct {
	id, userID, kind, target, recipient string
	attempts                            int
}

// Dispatch runs at most one due job.
func (o *Outbox) Dispatch(ctx context.Context) error {
	j, ok, err := o.claim(ctx)
	if err != nil || !ok {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	return o.finish(ctx, j, o.run(runCtx, j))
}

// claim leases the next due job. The lease is taken in the same statement that counts the attempt.
func (o *Outbox) claim(ctx context.Context) (job, bool, error) {
	var j job
	err := o.db.SQL().QueryRowContext(ctx, `UPDATE account_deletion_jobs SET lease_until=now()+make_interval(secs => $1), attempts=attempts+1
		WHERE id=(SELECT id FROM account_deletion_jobs WHERE state='queued' AND attempts<$2 AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<now())
			ORDER BY next_attempt_at, id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, user_id, kind, target, recipient, attempts`, int64(jobLease/time.Second), maxJobAttempts).Scan(&j.id, &j.userID, &j.kind, &j.target, &j.recipient, &j.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return job{}, false, nil
	}
	if err != nil {
		return job{}, false, fmt.Errorf("account deletion outbox: claim job: %w", err)
	}
	return j, true, nil
}

// run performs one job. The confirmation mail is queued in the mail outbox with a stable id, so a
// retry after a crash cannot send it twice.
func (o *Outbox) run(ctx context.Context, j job) error {
	switch j.kind {
	case jobWorkOSUser:
		if o.workos == nil {
			return errors.New("WorkOS is not configured")
		}
		return o.workos.DeleteWorkOSUser(ctx, j.target)
	case jobStripeCancel:
		if o.stripe == nil {
			return errors.New("Stripe is not configured")
		}
		return o.stripe.CancelSubscription(ctx, j.target)
	case jobConfirmation:
		return o.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, err := transactionalmail.EnqueueOnceTx(ctx, tx, "account-deleted:"+j.userID, transactionalmail.Message{
				From:    o.from,
				To:      []string{j.recipient},
				Subject: "Your Paperboat account was deleted",
				HTML: "<p>Your Paperboat account was deleted. Its machines, environment variables, previews and tunnels were removed, and its sessions and API tokens ended.</p>" +
					"<p>Invoices and payment records are kept as tax law requires. You can create a new account with " + html.EscapeString(j.recipient) + " at any time.</p>",
			})
			return err
		})
	default:
		return fmt.Errorf("unknown account deletion job kind %q", j.kind)
	}
}

// finish records the outcome. A job that succeeded drops its recipient; a failure is retried after a
// backoff, or marked failed once its attempts are used up. The error text stays out of the table.
func (o *Outbox) finish(ctx context.Context, j job, runErr error) error {
	if runErr == nil {
		_, err := o.db.SQL().ExecContext(ctx, `UPDATE account_deletion_jobs SET state='done', completed_at=now(), recipient='', lease_until=NULL, last_error=''
			WHERE id=$1`, j.id)
		if err != nil {
			return fmt.Errorf("account deletion outbox: mark job done: %w", err)
		}
		return nil
	}
	state := "queued"
	if j.attempts >= maxJobAttempts {
		state = "failed"
	}
	_, err := o.db.SQL().ExecContext(ctx, `UPDATE account_deletion_jobs SET state=$2, lease_until=NULL, last_error=$3,
		next_attempt_at=now()+make_interval(secs => $4) WHERE id=$1`, j.id, state, j.kind+" failed", int64(o.backoff(j.attempts)/time.Second))
	if err != nil {
		return fmt.Errorf("account deletion outbox: record failed job: %w", err)
	}
	return nil
}

// backoff doubles from one minute and stops at maxJobBackoff.
func (o *Outbox) backoff(attempts int) time.Duration {
	delay := time.Minute << min(max(attempts-1, 0), 10)
	return min(delay, maxJobBackoff)
}
