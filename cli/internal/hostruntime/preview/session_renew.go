package preview

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// LeaseMutation applies an authenticated projection through the session owner.
type LeaseMutation func(func(Lease) (Lease, error)) (Lease, error)

func (s *Session) renewLoop(ctx context.Context) error {
	attempt := 0
	for {
		lease := s.currentLease()
		expiresAt := lease.LeaseDeadline
		if lease.UserDeadline != nil && lease.UserDeadline.Before(expiresAt) {
			expiresAt = *lease.UserDeadline
		}
		remaining := expiresAt.Sub(s.config.Now().UTC())
		if remaining <= 0 {
			return ErrLeaseExpired
		}
		next := s.config.RenewInterval
		if remaining/2 < next {
			next = remaining / 2
		}
		if next <= 0 {
			return ErrLeaseExpired
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
		lease = s.currentLease()
		expiresAt = lease.LeaseDeadline
		if lease.UserDeadline != nil && lease.UserDeadline.Before(expiresAt) {
			expiresAt = *lease.UserDeadline
		}
		if !expiresAt.After(s.config.Now().UTC()) {
			return ErrLeaseExpired
		}
		renewIdempotencyKey, keyErr := newSessionIdempotencyKey(s.config.Random)
		if keyErr != nil {
			return fmt.Errorf("%w: generate renew idempotency key: %w", ErrLeaseLost, keyErr)
		}
		for {
			renewCtx, cancelRenew := context.WithDeadline(ctx, expiresAt)
			_, err := s.renewLease(renewCtx, renewIdempotencyKey)
			cancelRenew()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if errors.Is(err, context.DeadlineExceeded) && !expiresAt.After(s.config.Now().UTC()) {
					return ErrLeaseExpired
				}
				if !retryableLeaseError(err) {
					return errors.Join(ErrLeaseLost, err)
				}
				remaining = expiresAt.Sub(s.config.Now().UTC())
				if remaining <= 0 {
					return ErrLeaseExpired
				}
				delay := reconnectDelay(s.config.ReconnectBackoff, s.config.MaxReconnectBackoff, attempt)
				if delay > remaining {
					delay = remaining
				}
				if err := waitReconnect(ctx, delay); err != nil {
					return nil
				}
				if !expiresAt.After(s.config.Now().UTC()) {
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
		return Lease{}, err
	}
	observed.CreateOperationID = previous.CreateOperationID
	observed.Generation = leaseGenerationForID(observed.ID, observed.ETag)
	if !sameLeaseIdentity(previous, observed) || !validActiveLeaseState(observed.State) || observed.Generation < previous.Generation {
		return Lease{}, ErrSessionInvalid
	}
	if err := validateSessionLease(observed, s.config, s.config.Now()); err != nil {
		return Lease{}, err
	}
	if observed.Generation > previous.Generation {
		if err := s.acceptRenewal(observed, previous); err != nil {
			return Lease{}, err
		}
	}
	if publish && isReadyLease(observed) {
		if err := s.markReady(observed); err != nil {
			return Lease{}, err
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
			return fmt.Errorf("%w: manager rejected renewed lease: %v", ErrLeaseLost, err)
		}
	}
	if s.config.OnRenew != nil {
		s.config.OnRenew(renewed)
	}
	return nil
}
