//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// managedSSHService owns optional SSH initialization independently from the
// machine's terminal and other services. Admission remains controlled by the
// authenticated machine policy; local reachability grants no authority.
type managedSSHService struct {
	disable           chan chan error
	attemptCancel     context.CancelFunc
	gate              server.CapabilityGate
	initialize        func(context.Context) (Service, error)
	cleanup           func(context.Context) error
	interval, timeout time.Duration
	mu                sync.Mutex
	cancel            context.CancelFunc
	done              chan struct{}
	health            health.Capability
	shutdownErr       error
}

func newProductionManagedSSH(controlURL string, transport http.RoundTripper, registration runtimeidentity.Registration, identity managedSSHIdentitySource, generation uint64, gate server.CapabilityGate) (*managedssh.Host, Service, error) {
	if registration.MachineID == "" || registration.InstallationGeneration < 1 || identity == nil || generation == 0 || gate == nil {
		return nil, nil, ErrProductionInvalid
	}
	if registration.SSHUser == "" && registration.SSHPort == 0 {
		return nil, nil, nil
	}
	if registration.SSHUser == "" || registration.SSHPort == 0 {
		return nil, nil, ErrManagedSSHUnavailable
	}
	host, err := managedssh.NewHost(managedssh.HostConfig{MaxStreams: 32, ProbeTimeout: 3 * time.Second, DialTimeout: 10 * time.Second})
	if err != nil {
		return nil, nil, err
	}
	service := &managedSSHService{gate: gate, interval: 2 * time.Second, timeout: 45 * time.Second, cleanup: func(ctx context.Context) error { return cleanupProductionManagedSSH(ctx, registration) }, initialize: func(ctx context.Context) (Service, error) {
		return initializeProductionManagedSSH(ctx, host, controlURL, transport, registration, identity, generation)
	}}
	return host, service, nil
}

func (s *managedSSHService) Start(ctx context.Context) error {
	if ctx == nil || s.gate == nil || s.initialize == nil || s.interval <= 0 || s.timeout <= 0 {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return ErrProductionInvalid
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel, s.done = cancel, make(chan struct{})
	s.disable = make(chan chan error)
	s.health = health.Capability{State: health.Unavailable, Reason: "disabled"}
	go s.run(runCtx)
	return nil
}

func (s *managedSSHService) run(ctx context.Context) {
	defer close(s.done)
	var authority Service
	cleanupPending := false
	localCleanupPending := true
	stopAuthority := func() error {
		if authority == nil {
			return nil
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		err := authority.Shutdown(shutdownCtx)
		if err == nil {
			authority = nil
		}
		return err
	}
	defer func() { err := stopAuthority(); s.mu.Lock(); s.shutdownErr = err; s.mu.Unlock() }()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		if localCleanupPending {
			if err := s.cleanupPersisted(ctx); err != nil {
				s.setHealth(health.Unavailable, "cleanup_failed")
			} else {
				localCleanupPending = false
			}
		}
		if cleanupPending {
			if err := stopAuthority(); err != nil {
				s.setHealth(health.Unavailable, "cleanup_failed")
			} else {
				cleanupPending = false
			}
		}
		if !s.gate.Enabled("ssh.v1") {
			err := stopAuthority()
			if err != nil {
				s.setHealth(health.Unavailable, "cleanup_failed")
			} else {
				if !localCleanupPending {
					s.setHealth(health.Unavailable, "disabled")
				}
			}
		} else if authority == nil && !cleanupPending && !localCleanupPending {
			attemptCtx, cancel := context.WithTimeout(ctx, s.timeout)
			s.mu.Lock()
			s.attemptCancel = cancel
			s.mu.Unlock()
			candidate, err := s.initialize(attemptCtx)
			if err == nil && candidate == nil {
				err = ErrManagedSSHUnavailable
			}
			if err == nil && !s.gate.Enabled("ssh.v1") {
				err = context.Canceled
			}
			if err == nil {
				err = candidate.Start(ctx)
			}
			cancel()
			s.mu.Lock()
			s.attemptCancel = nil
			s.mu.Unlock()
			if err != nil {
				if candidate != nil {
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), s.timeout)
					cleanupErr := candidate.Shutdown(shutdownCtx)
					err = errors.Join(err, cleanupErr)
					if cleanupErr != nil {
						authority, cleanupPending = candidate, true
					}
					shutdownCancel()
				}
				s.setHealth(health.Unavailable, "target_or_authority_unavailable")
			} else {
				authority = candidate
				s.setHealth(health.Ready, "")
			}
		}
		if authority != nil && !cleanupPending && s.gate.Enabled("ssh.v1") {
			if provider, ok := authority.(capabilityHealthProvider); ok {
				current := provider.CapabilityHealth()
				s.mu.Lock()
				s.health = current
				s.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case acknowledgement := <-s.disable:
			err := errors.Join(stopAuthority(), s.cleanupPersisted(ctx))
			if err != nil {
				cleanupPending = true
				localCleanupPending = true
				s.setHealth(health.Unavailable, "cleanup_failed")
			} else {
				s.setHealth(health.Unavailable, "disabled")
			}
			acknowledgement <- err
		case <-ticker.C:
		}
	}
}

func (s *managedSSHService) setHealth(state health.State, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health = health.Capability{State: state, Reason: reason}
	if state != health.Ready && reason != "disabled" {
		s.health.RetryAfterMs = uint64(s.interval / time.Millisecond)
	}
}
func (s *managedSSHService) CapabilityHealth() health.Capability {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}
func (s *managedSSHService) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *managedSSHService) Disable(ctx context.Context) error {
	if ctx == nil {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	cancel, attemptCancel, done, requests := s.cancel, s.attemptCancel, s.done, s.disable
	s.mu.Unlock()
	if cancel == nil {
		return s.cleanupPersisted(ctx)
	}
	if attemptCancel != nil {
		attemptCancel()
	}
	acknowledgement := make(chan error, 1)
	select {
	case requests <- acknowledgement:
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-acknowledgement:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *managedSSHService) cleanupPersisted(ctx context.Context) error {
	if s.cleanup == nil {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.cleanup(bounded)
}
