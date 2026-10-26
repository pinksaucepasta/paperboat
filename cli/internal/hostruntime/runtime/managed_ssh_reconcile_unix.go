//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

type managedSSHKeyReconciler struct {
	client           managedSSHControlClient
	identity         managedSSHIdentitySource
	registration     runtimeidentity.Registration
	workerGeneration uint64
	publicKeys       []string
	// readPublicKeys re-reads the host's SSH public keys. While the server refuses them as another
	// machine's, a changed set (the owner regenerated them) is published at the next tick.
	readPublicKeys func() ([]string, error)
	refusedAt      time.Time
	home           string
	ownerUID       uint32
	interval       time.Duration
	timeout        time.Duration
	events         *machineevents.Subscription
	sequence       atomic.Uint64
	ready          atomic.Bool
	hostKeyInUse   atomic.Bool

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *managedSSHKeyReconciler) Start(ctx context.Context) error {
	if ctx == nil || s.client == nil || s.identity == nil || s.registration.MachineID == "" || s.registration.InstallationGeneration < 1 || s.workerGeneration == 0 || len(s.publicKeys) == 0 || s.home == "" || s.interval <= 0 || s.timeout <= 0 {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errors.New("managed SSH key reconciliation is already running")
	}
	initialCtx, initialCancel := context.WithTimeout(ctx, s.timeout)
	err := s.reconcile(initialCtx)
	initialCancel()
	if ctx.Err() != nil {
		return errors.Join(ErrManagedSSHUnavailable, ctx.Err(), err)
	}
	if err != nil {
		var unavailable *managedSSHAuthorityUnavailable
		var inUse *managedSSHHostKeyInUse
		switch {
		case errors.As(err, &inUse):
			// Everything except managed SSH keeps working; the owner regenerates this machine's keys.
			slog.Warn("managed SSH disabled: this machine's SSH host keys are already another machine's; regenerate them, then run pb repair")
		case !errors.As(err, &unavailable):
			return errors.Join(ErrManagedSSHUnavailable, err)
		default:
			slog.Warn("managed SSH authority unavailable at startup; managed keys removed, retrying")
		}
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(workerCtx, s.done)
	return nil
}

// run asks the server again when a key change is published (machine_access), after a failure, every
// interval while change delivery is down, and otherwise every managedSSHKeyFallback (PR-03).
func (s *managedSSHKeyReconciler) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	failed := false
	for {
		timer := time.NewTimer(s.nextReconcile(failed))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.events.Changed():
			timer.Stop()
		case <-timer.C:
		}
		if !s.hostKeysChangedOrRetryDue(time.Now()) {
			continue
		}
		reconcileCtx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.reconcile(reconcileCtx)
		cancel()
		// An unreachable authority keeps the last verified keys (reconcile); an explicit refusal
		// removed them.
		if failed = err != nil; failed {
			slog.Warn("managed SSH authority refresh failed", "error", err)
		}
	}
}

func (s *managedSSHKeyReconciler) reconcile(ctx context.Context) error {
	s.ready.Store(false)
	sequence := s.sequence.Add(1)
	suffix := s.registration.MachineID + "-" + strconv.FormatUint(uint64(s.registration.InstallationGeneration), 10) + "-" + strconv.FormatUint(s.workerGeneration, 10) + "-refresh-" + strconv.FormatUint(sequence, 10)
	keys, active, err := reconcileManagedSSHAuthorityWithOperations(ctx, s.client, s.identity, s.registration, s.workerGeneration, s.publicKeys, "managed-ssh-observe-"+suffix, "managed-ssh-keys-"+suffix)
	s.hostKeyInUse.Store(hostKeysInUseElsewhere(err))
	if s.hostKeyInUse.Load() {
		s.refusedAt = time.Now()
	}
	if err != nil {
		// Paperboat unreachable: the last verified keys stay until a revocation the host receives, as
		// the peer network does (user decision "offline until revoked", F5-08). An explicit answer
		// (refused, revoked, host keys in use, invalid) still removes them.
		if !s.hostKeyInUse.Load() && retryableManagedSSHAuthorityError(err) {
			return &managedSSHAuthorityUnavailable{cause: err}
		}
		_, cleanupErr := reconcilePlatformAuthorizedKeys(s.home, s.ownerUID, nil)
		if cleanupErr == nil && s.hostKeyInUse.Load() {
			return &managedSSHHostKeyInUse{cause: err}
		}
		return errors.Join(err, cleanupErr)
	}
	if !active {
		keys.Keys = nil
	}
	_, err = reconcilePlatformAuthorizedKeys(s.home, s.ownerUID, keys.Keys)
	s.ready.Store(err == nil && active)
	return err
}

func (s *managedSSHKeyReconciler) Shutdown(ctx context.Context) error {
	s.ready.Store(false)
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	// The last verified keys stay across a restart (F5-08); turning managed SSH off removes them
	// (managedSSHService.cleanupPersisted).
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Only availability failures may keep initial reconciliation alive, and they keep the last verified
// keys. Explicit authentication/authorization rejection and invalid local or protocol state fail
// startup and remove them.
type managedSSHAuthorityUnavailable struct{ cause error }

func (e *managedSSHAuthorityUnavailable) Error() string {
	return "managed SSH authority is temporarily unavailable"
}
func (e *managedSSHAuthorityUnavailable) Unwrap() error { return e.cause }
func retryableManagedSSHAuthorityError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, clientapi.ErrUnauthenticated) {
		return false
	}
	var response *clientapi.APIError
	if errors.As(err, &response) {
		return response.Status == http.StatusRequestTimeout || response.Status == http.StatusTooManyRequests || response.Status >= 500 && response.Status <= 599
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var network *net.OpError
	return errors.As(err, &network)
}

func (s *managedSSHKeyReconciler) CapabilityHealth() health.Capability {
	if s.ready.Load() {
		return health.Capability{State: health.Ready}
	}
	if s.hostKeyInUse.Load() {
		return health.Capability{State: health.Unavailable, Reason: managedSSHHostKeyInUseCode, RetryAfterMs: uint64(managedSSHHostKeyInUseRetry / time.Millisecond)}
	}
	return health.Capability{State: health.Unavailable, Reason: "authority_unavailable", RetryAfterMs: uint64(s.interval / time.Millisecond)}
}

// managedSSHHostKeyInUseCode is the server's refusal of host keys another machine already uses (a
// cloned VM or container image). It does not change by retrying, so the machine asks rarely.
const managedSSHHostKeyInUseCode = "ssh_host_key_in_use"

const managedSSHHostKeyInUseRetry = 10 * time.Minute

type managedSSHHostKeyInUse struct{ cause error }

func (e *managedSSHHostKeyInUse) Error() string {
	return "this machine's SSH host keys are already another machine's"
}
func (e *managedSSHHostKeyInUse) Unwrap() error { return e.cause }

func hostKeysInUseElsewhere(err error) bool {
	var response *clientapi.APIError
	return errors.As(err, &response) && response.Status == http.StatusConflict && response.Code == managedSSHHostKeyInUseCode
}

// managedSSHKeyFallback is how often a host still asks for its keys while key changes are delivered. A
// key also leaves the set without a write when its CLI session goes unused past the active window
// (days), so this bounds how late such a key is removed.
const managedSSHKeyFallback = 10 * time.Minute

func (s *managedSSHKeyReconciler) nextReconcile(failed bool) time.Duration {
	if failed || !s.events.Healthy() {
		return s.interval
	}
	return managedSSHKeyFallback
}

// hostKeysChangedOrRetryDue reports whether a tick should reconcile. Refused host keys do not change
// by asking again, so the server is asked again only when the keys on disk differ from the refused
// ones (adopting them) or managedSSHHostKeyInUseRetry has passed.
func (s *managedSSHKeyReconciler) hostKeysChangedOrRetryDue(now time.Time) bool {
	if !s.hostKeyInUse.Load() {
		return true
	}
	if s.readPublicKeys != nil {
		if keys, err := s.readPublicKeys(); err == nil && len(keys) > 0 && !slices.Equal(keys, s.publicKeys) {
			s.publicKeys = keys
			return true
		}
	}
	return now.Sub(s.refusedAt) >= managedSSHHostKeyInUseRetry
}
