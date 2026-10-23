package accountdeletion

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeProviders records provider calls. While a failure is set, the matching call fails.
type fakeProviders struct {
	workosErr   error
	stripeErr   error
	workosCalls []string
	stripeCalls []string
}

func (f *fakeProviders) DeleteWorkOSUser(_ context.Context, subject string) error {
	f.workosCalls = append(f.workosCalls, subject)
	return f.workosErr
}

func (f *fakeProviders) CancelSubscription(_ context.Context, id string) error {
	f.stripeCalls = append(f.stripeCalls, id)
	return f.stripeErr
}

// dispatchAll runs rounds of the outbox: each round makes every queued job due once, then dispatches until
// nothing is due. Dispatch takes one job per call and a failed job is scheduled after a backoff, so within a
// round each job runs once whatever order the random job ids give.
func (f *fixture) dispatchAll(outbox *Outbox, rounds int) {
	f.t.Helper()
	for i := 0; i < rounds; i++ {
		f.exec(`UPDATE account_deletion_jobs SET next_attempt_at=now() WHERE state='queued'`)
		for n := 0; n < 16; n++ {
			if f.count(`SELECT count(*) FROM account_deletion_jobs WHERE state='queued' AND next_attempt_at<=now() AND (lease_until IS NULL OR lease_until<now())`) == 0 {
				break
			}
			if err := outbox.Dispatch(f.t.Context()); err != nil {
				f.t.Fatal(err)
			}
		}
	}
}

// deletedAccount deletes an account and clears the queue of other tests' jobs, so only this account's run.
func (f *fixture) deletedAccount(name string) (u, subject string) {
	f.t.Helper()
	user := f.user(name)
	f.seedPersonal(user)
	if err := f.svc.Delete(f.t.Context(), user, time.Now(), user.PrimaryEmail); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`DELETE FROM account_deletion_jobs WHERE user_id<>$1`, user.ID)
	return user.ID, user.WorkOSSubject
}

func TestAccountDeletionOutboxRetriesWithoutBlockingDeletion(t *testing.T) {
	f := newFixture(t)
	userID, subject := f.deletedAccount("retry")
	providers := &fakeProviders{workosErr: errors.New("provider down")}
	outbox := f.svc.Worker(providers, providers)

	f.dispatchAll(outbox, 4)

	if got := f.count(`SELECT count(*) FROM users WHERE id=$1 AND status='deleted'`, userID); got != 1 {
		t.Fatal("a failing provider reverted the deletion")
	}
	if len(providers.stripeCalls) != 1 || providers.stripeCalls[0] != "sub_"+userID {
		t.Fatalf("subscription cancel calls = %v", providers.stripeCalls)
	}
	if got := f.count(`SELECT count(*) FROM transactional_mail_outbox WHERE mail_id=$1`, "account-deleted:"+userID); got != 1 {
		t.Fatalf("confirmation mail queued %d times, want 1", got)
	}
	var state, lastError string
	var attempts int
	var due bool
	if err := f.store.SQL().QueryRowContext(t.Context(), `SELECT state, attempts, last_error, next_attempt_at > now() FROM account_deletion_jobs WHERE user_id=$1 AND kind='workos_user'`, userID).Scan(&state, &attempts, &lastError, &due); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attempts < 1 || lastError != "workos_user failed" || !due {
		t.Fatalf("failed WorkOS job: state=%s attempts=%d last_error=%q backing_off=%v", state, attempts, lastError, due)
	}
	if got := f.count(`SELECT count(*) FROM account_deletion_jobs WHERE user_id=$1 AND kind='stripe_subscription' AND state='done'`, userID); got != 1 {
		t.Fatal("the subscription job did not finish")
	}

	providers.workosErr = nil
	f.dispatchAll(outbox, 2)
	if len(providers.workosCalls) < 2 || providers.workosCalls[len(providers.workosCalls)-1] != subject {
		t.Fatalf("WorkOS calls = %v", providers.workosCalls)
	}
	if got := f.count(`SELECT count(*) FROM account_deletion_jobs WHERE user_id=$1 AND state='done' AND recipient=''`, userID); got != 3 {
		t.Fatalf("finished jobs = %d, want 3 (the mail job drops its recipient)", got)
	}
}

func TestAccountDeletionOutboxMailIsQueuedOnce(t *testing.T) {
	f := newFixture(t)
	userID, _ := f.deletedAccount("mailonce")
	providers := &fakeProviders{}
	outbox := f.svc.Worker(providers, providers)
	f.dispatchAll(outbox, 3)
	f.exec(`UPDATE account_deletion_jobs SET state='queued', next_attempt_at=now() WHERE user_id=$1 AND kind='confirmation_mail'`, userID)
	f.dispatchAll(outbox, 1)
	if got := f.count(`SELECT count(*) FROM transactional_mail_outbox WHERE mail_id=$1`, "account-deleted:"+userID); got != 1 {
		t.Fatalf("a retried confirmation queued %d mails, want 1", got)
	}
}

func TestAccountDeletionOutboxStopsAtTheAttemptBound(t *testing.T) {
	f := newFixture(t)
	userID, _ := f.deletedAccount("bound")
	providers := &fakeProviders{stripeErr: errors.New("provider down")}
	outbox := f.svc.Worker(providers, providers)
	f.exec(`UPDATE account_deletion_jobs SET state='done' WHERE user_id=$1 AND kind<>'stripe_subscription'`, userID)
	f.exec(`UPDATE account_deletion_jobs SET attempts=$2 WHERE user_id=$1 AND kind='stripe_subscription'`, userID, maxJobAttempts-1)
	f.dispatchAll(outbox, 1)
	if got := f.count(`SELECT count(*) FROM account_deletion_jobs WHERE user_id=$1 AND kind='stripe_subscription' AND state='failed'`, userID); got != 1 {
		t.Fatal("the last attempt did not mark the job failed")
	}
	f.dispatchAll(outbox, 1)
	if len(providers.stripeCalls) != 1 {
		t.Fatalf("a failed job was run again: %v", providers.stripeCalls)
	}
}

func TestAccountDeletionOutboxBackoffIsBounded(t *testing.T) {
	outbox := &Outbox{}
	if got := outbox.backoff(1); got != time.Minute {
		t.Fatalf("first retry waits %s", got)
	}
	if got := outbox.backoff(50); got != maxJobBackoff {
		t.Fatalf("late retry waits %s, want %s", got, maxJobBackoff)
	}
}
