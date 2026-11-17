package preview

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// ownerRefusalWindow is how long renewals that the server refuses with 401, 403 or 412 are retried. A host just back
// from sleep is refused (401: its short-lived machine credential expired during the sleep and is not renewed yet;
// 403: its presence is not yet re-established; 412: a readiness step moved the generation) until it is online again,
// a few seconds later (X-26; the 401 ended a background preview at once on 2026-11-16).
// shortcut: fixed 2 min, a revoked machine retries that long.
var ownerRefusalWindow = 2 * time.Minute

// renewAttemptTimeout bounds one renew request of a foreground session, which has no deadline of its own.
const renewAttemptTimeout = 30 * time.Second

func ownerRefusal(err error) bool {
	var refusal *api.APIError
	return errors.As(err, &refusal) && (refusal.Status == http.StatusUnauthorized ||
		refusal.Status == http.StatusForbidden || refusal.Status == http.StatusPreconditionFailed)
}

// LeaseMutation applies an authenticated projection through the session owner.
type LeaseMutation func(func(Lease) (Lease, error)) (Lease, error)

// leaseEnd is the earlier of the lease and user deadlines. bounded is false for a foreground lease (no user
// deadline): it persists for as long as its command runs, however long the machine sleeps, so this machine's
// wall clock passing LeaseDeadline during a sleep must not end it before the first renew after the wake reaches
// the server. Only the server's answer (stopped, expired, owner_disconnected, 404 or a refusal that outlasts
// ownerRefusalWindow) ends it.
func leaseEnd(lease Lease) (end time.Time, bounded bool) {
	end = lease.LeaseDeadline
	if lease.UserDeadline != nil && lease.UserDeadline.Before(end) {
		end = *lease.UserDeadline
	}
	return end, lease.UserDeadline != nil
}

func (s *Session) renewLoop(ctx context.Context) error {
	attempt := 0
	for {
		expiresAt, bounded := leaseEnd(s.currentLease())
		remaining := expiresAt.Sub(s.config.Now().UTC())
		if bounded && remaining <= 0 {
			return ErrLeaseExpired
		}
		next := s.config.RenewInterval
		if remaining/2 < next {
			next = remaining / 2
		}
		if next <= 0 {
			if bounded {
				return ErrLeaseExpired
			}
			next = 0 // a foreground lease already past its deadline (a wake): renew at once
		}
		timer := time.NewTimer(next)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}

		if ctx.Err() != nil {
			return nil
		}
		// Readiness can advance the lease generation while the renewal timer
		// is running.  Always send the latest lease projection so the renew
		// CAS is based on the ETag that readiness installed, rather than the
		// projection captured before the wait.
		expiresAt, bounded = leaseEnd(s.currentLease())
		if bounded && !expiresAt.After(s.config.Now().UTC()) {
			return ErrLeaseExpired
		}
		renewIdempotencyKey, keyErr := newSessionIdempotencyKey(s.config.Random)
		if keyErr != nil {
			return fmt.Errorf("%w: generate renew idempotency key: %w", ErrLeaseLost, keyErr)
		}
		var refusedSince time.Time
		for {
			renewCtx, cancelRenew := context.WithTimeout(ctx, renewAttemptTimeout)
			if bounded {
				cancelRenew()
				renewCtx, cancelRenew = context.WithDeadline(ctx, expiresAt)
			}
			_, err := s.renewLease(renewCtx, renewIdempotencyKey)
			cancelRenew()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if bounded && errors.Is(err, context.DeadlineExceeded) && !expiresAt.After(s.config.Now().UTC()) {
					return ErrLeaseExpired
				}
				if !retryableLeaseError(err) {
					if !ownerRefusal(err) {
						return errors.Join(ErrLeaseLost, err)
					}
					if refusedSince.IsZero() {
						refusedSince = s.config.Now()
					}
					if s.config.Now().Sub(refusedSince) >= ownerRefusalWindow {
						return errors.Join(ErrLeaseLost, err)
					}
					// The refused request changed nothing, so the retry is a new request, not a replay.
					if renewIdempotencyKey, keyErr = newSessionIdempotencyKey(s.config.Random); keyErr != nil {
						return fmt.Errorf("%w: generate renew idempotency key: %w", ErrLeaseLost, keyErr)
					}
				}
				delay := reconnectDelay(s.config.ReconnectBackoff, s.config.MaxReconnectBackoff, attempt)
				if bounded {
					remaining = expiresAt.Sub(s.config.Now().UTC())
					if remaining <= 0 {
						return ErrLeaseExpired
					}
					if delay > remaining {
						delay = remaining
					}
				}
				if err := waitReconnect(ctx, delay); err != nil {
					return nil
				}
				if bounded && !expiresAt.After(s.config.Now().UTC()) {
					return ErrLeaseExpired
				}
				attempt++
				continue
			}
			attempt = 0
			break
		}
	}
}

func (s *Session) renewLease(ctx context.Context, idempotencyKey string) (Lease, error) {
	type result struct {
		lease Lease
		err   error
	}
	completed := make(chan result, 1)
	go func() {
		renewed, err := s.mutateLease(func(current Lease) (Lease, error) {
			return s.config.LeaseClient.Renew(ctx, current, idempotencyKey)
		}, false)
		completed <- result{lease: renewed, err: err}
	}()
	select {
	case outcome := <-completed:
		return outcome.lease, outcome.err
	case <-ctx.Done():
		return Lease{}, ctx.Err()
	}
}

// Server mutations and readiness reads use one canonical lease owner. Adopt
// pending publication before releasing a mutation, so renewal never uses the
// pre-report ETag. A late completion cannot replace a newer generation.
func (s *Session) mutateLease(update func(Lease) (Lease, error), publish bool) (Lease, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	previous := s.currentLease()
	observed, err := update(previous)
	if err != nil {
		return Lease{}, readinessOwnerFailed("owner_update", err)
	}
	observed.CreateOperationID = previous.CreateOperationID
	observed.Generation = leaseGenerationForID(observed.ID, observed.ETag)
	if !sameLeaseIdentity(previous, observed) || !validActiveLeaseState(observed.State) || observed.Generation < previous.Generation {
		return Lease{}, readinessOwnerFailed("owner_projection", ErrSessionInvalid)
	}
	if err := validateSessionLease(observed, s.config, s.config.Now()); err != nil {
		return Lease{}, readinessOwnerFailed("owner_validate", err)
	}
	if observed.Generation > previous.Generation {
		if err := s.acceptRenewal(observed, previous); err != nil {
			return Lease{}, readinessOwnerFailed("owner_accept_renewal", err)
		}
	}
	if publish && isReadyLease(observed) {
		if err := s.markReady(observed); err != nil {
			return Lease{}, readinessOwnerFailed("owner_mark_ready", err)
		}
	}
	return observed, nil
}

func (s *Session) mutateReadiness(update func(Lease) (Lease, error)) (Lease, error) {
	return s.mutateLease(update, true)
}

func (s *Session) acceptRenewal(renewed, previous Lease) error {
	if !sameLeaseIdentity(previous, renewed) {
		return fmt.Errorf("%w: renewal changed lease identity or owner", ErrSessionInvalid)
	}
	if !validActiveLeaseState(renewed.State) {
		return fmt.Errorf("%w: server marked the lease %s", ErrLeaseLost, renewed.State)
	}
	previousGeneration := previous.Generation
	if previousGeneration < 1 {
		previousGeneration = leaseGenerationForID(previous.ID, previous.ETag)
	}
	newGeneration := leaseGenerationForID(renewed.ID, renewed.ETag)
	if renewed.ETag == previous.ETag || newGeneration <= previousGeneration {
		return fmt.Errorf("%w: renewal did not advance the lease generation", ErrSessionInvalid)
	}
	if err := validateSessionLease(renewed, s.config, s.config.Now()); err != nil {
		return err
	}
	s.mu.Lock()
	renewed.Generation = newGeneration
	renewed.LazyLifecycle = previous.LazyLifecycle
	s.lease = renewed
	s.mu.Unlock()
	s.managerMu.Lock()
	managers := make([]*SessionManager, 0, len(s.managers))
	for manager := range s.managers {
		managers = append(managers, manager)
	}
	s.managerMu.Unlock()
	for _, manager := range managers {
		if err := manager.rekey(s, renewed); err != nil {
			s.cancel()
			return readinessOwnerFailed("owner_renewal_rekey", fmt.Errorf("%w: manager rejected renewed lease: %v", ErrLeaseLost, err))
		}
	}
	if s.config.OnRenew != nil {
		s.config.OnRenew(renewed)
	}
	return nil
}
