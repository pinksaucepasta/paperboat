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
	// watchHostKeys signals changes to the host key files (nil: poll them every interval). While the watch
	// is open the files are re-read on a signal, and otherwise only at the fallback; a watch that cannot be
	// opened, or ends, leaves the poll in charge and is opened again at the next interval.
	watchHostKeys func(context.Context) (<-chan struct{}, error)
	keyWatch      <-chan struct{}
	keyWatchLost  bool
	refusedAt     time.Time
	refusals      atomic.Uint32 // consecutive refusals of the host keys as another machine's: sets the next retry (inUseDelay)
	// keyChanges counts host-key sets re-read after start. The server keeps one set per observation
	// generation, so each changed set is observed under its own generation (observationGeneration).
	keyChanges   uint64
	home         string
	ownerUID     uint32
	interval     time.Duration
	timeout      time.Duration
	events       *machineevents.Subscription
	sequence     atomic.Uint64
	ready        atomic.Bool
	hostKeyInUse atomic.Bool

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
	s.openKeyWatch(workerCtx)
	go s.run(workerCtx, s.done)
	return nil
}

// run re-reads the host keys every interval (a local read) and asks the server when they changed (C066:
// the server must hold regenerated keys as pending for trust-host), when a key change is published
// (machine_access), after a failure, every interval while change delivery is down, and otherwise every
// managedSSHKeyFallback (PR-03).
func (s *managedSSHKeyReconciler) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	failed := false
	last := time.Now()
	for {
		timer := time.NewTimer(s.interval)
		published, keysSignalled := false, false
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.events.Changed():
			timer.Stop()
			published = true
		case _, open := <-s.keyWatch:
			timer.Stop()
			keysSignalled = true
			if !open {
				s.keyWatch = nil
			}
		case <-timer.C:
			// Keys changed while no watch was open are caught by the read this opens it with.
			keysSignalled = s.keyWatch == nil && s.openKeyWatch(ctx)
		}
		now := time.Now()
		if !s.reconcileDue(now, last, failed, published, keysSignalled) {
			continue
		}
		reconcileCtx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.reconcile(reconcileCtx)
		cancel()
		last = now
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
	keys, active, err := reconcileManagedSSHAuthorityWithOperations(ctx, s.client, s.identity, s.registration, s.observationGeneration(), s.publicKeys, "managed-ssh-observe-"+suffix, "managed-ssh-keys-"+suffix)
	s.hostKeyInUse.Store(hostKeysInUseElsewhere(err))
	if s.hostKeyInUse.Load() {
		s.refusedAt = time.Now()
		s.refusals.Add(1)
	} else {
		s.refusals.Store(0)
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
	// Not active (pending): the keys are the server's current client keys, kept so trust-host works at once.
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
	// Any network failure: a refused or timed-out connect (*net.OpError), a failed lookup (*net.DNSError), an HTTP client
	// timeout (*url.Error). Paperboat unreachable keeps the last verified keys until a revocation the host receives (F5-08);
	// only an explicit answer removes them. Matching *net.OpError alone dropped them on a lookup failure.
	var network net.Error
	return errors.As(err, &network)
}

func (s *managedSSHKeyReconciler) CapabilityHealth() health.Capability {
	if s.ready.Load() {
		return health.Capability{State: health.Ready}
	}
	if s.hostKeyInUse.Load() {
		return health.Capability{State: health.Unavailable, Reason: managedSSHHostKeyInUseCode, RetryAfterMs: uint64(s.inUseDelay() / time.Millisecond)}
	}
	return health.Capability{State: health.Unavailable, Reason: "authority_unavailable", RetryAfterMs: uint64(s.interval / time.Millisecond)}
}

// managedSSHHostKeyInUseCode is the server's refusal of host keys another machine already uses (a
// cloned VM or container image, or the live record of this same computer before its reset or reinstall
// replaces it). Asking again changes nothing for a clone, but the same computer enrolled again is
// accepted as soon as its old record is removed, so the first retry comes quickly and each later one
// waits twice as long, up to managedSSHHostKeyInUseRetry (a cloned machine ends up asking rarely).
const managedSSHHostKeyInUseCode = "ssh_host_key_in_use"

const (
	managedSSHHostKeyInUseFirstRetry = 30 * time.Second
	managedSSHHostKeyInUseRetry      = 10 * time.Minute
)

// inUseDelay is how long after a refusal the host keys are asked for again: 30 s after the first, doubling
// per consecutive refusal.
func (s *managedSSHKeyReconciler) inUseDelay() time.Duration {
	delay := managedSSHHostKeyInUseFirstRetry
	for refused := s.refusals.Load(); refused > 1 && delay < managedSSHHostKeyInUseRetry; refused-- {
		delay *= 2
	}
	return min(delay, managedSSHHostKeyInUseRetry)
}

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

// observationGeneration is the worker generation for the keys read at start and, after the host keys
// changed, a value no worker generation reaches (boot generations count worker starts, far below 2^20),
// so the changed set is a new observation instead of a conflict with the first one (C066).
func (s *managedSSHKeyReconciler) observationGeneration() uint64 {
	if s.keyChanges == 0 {
		return s.workerGeneration
	}
	return s.workerGeneration<<20 | s.keyChanges
}

// reconcileDue reports whether this wake-up asks the server. Changed host keys on disk always do; they are read
// when the watch signalled, at the fallback, and every wake-up while no watch is open.
// Refused host keys are asked for again after inUseDelay, or at once when a key change is published for this machine
// (the server publishes one when it removes the record that held the keys).
func (s *managedSSHKeyReconciler) reconcileDue(now, last time.Time, failed, published, keysSignalled bool) bool {
	if s.readPublicKeys != nil && (keysSignalled || s.keyWatch == nil || now.Sub(last) >= managedSSHKeyFallback) {
		if keys, err := s.readPublicKeys(); err == nil && len(keys) > 0 && !slices.Equal(keys, s.publicKeys) {
			s.publicKeys = keys
			s.keyChanges++
			return true
		}
	}
	if s.hostKeyInUse.Load() {
		return published || now.Sub(s.refusedAt) >= s.inUseDelay()
	}
	return published || failed || !s.events.Healthy() || now.Sub(last) >= managedSSHKeyFallback
}

// openKeyWatch opens the host key watch when one is configured and none is open; it reports whether it opened one.
// A failure is logged once, not at every interval it is retried.
func (s *managedSSHKeyReconciler) openKeyWatch(ctx context.Context) bool {
	if s.watchHostKeys == nil || s.keyWatch != nil {
		return false
	}
	changes, err := s.watchHostKeys(ctx)
	if err != nil {
		if !s.keyWatchLost {
			slog.Warn("SSH host key watch unavailable; polling the key files instead", "error", err)
			s.keyWatchLost = true
		}
		return false
	}
	s.keyWatch, s.keyWatchLost = changes, false
	return true
}
