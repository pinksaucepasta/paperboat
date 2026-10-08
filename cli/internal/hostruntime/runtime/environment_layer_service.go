//go:build darwin || linux || windows

package runtime

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// layerEnvironmentService owns only host recipient custody and ciphertext
// delivery. Account passwords and personal/team keys never enter this service.
type layerEnvironmentService struct {
	layerRegistered bool
	layerRecipient  api.VaultLayerRecipient
	layerMu         sync.Mutex
	layers          *envinject.LayerStore
	mu              sync.RWMutex
	stateRoot       string
	base            *url.URL
	transport       http.RoundTripper
	registration    runtimeidentity.Registration
	// Helper identity authorizes key registration and launch-context reads.
	credentials managedSSHIdentitySource
	// Runtime observations require the installation machine-control identity.
	observationCredentials managedSSHIdentitySource
	cancel                 context.CancelFunc
	done                   chan struct{}
	refresh                sync.Mutex
	closed                 bool
}

func newLayerEnvironmentService(stateRoot string, base *url.URL, transport http.RoundTripper, registration runtimeidentity.Registration, credentials, observationCredentials managedSSHIdentitySource) *layerEnvironmentService {
	return &layerEnvironmentService{stateRoot: stateRoot, base: base, transport: transport, registration: registration, credentials: credentials, observationCredentials: observationCredentials, done: make(chan struct{})}
}
func (s *layerEnvironmentService) Start(ctx context.Context) error {
	if ctx == nil || s == nil || s.credentials == nil || s.observationCredentials == nil {
		return ErrProductionInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Startup context cancellation must not stop accepted host-owned custody.
	// Shutdown owns cancellation and waits for the registration loop.
	run, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.cancel != nil || s.closed {
		s.mu.Unlock()
		cancel()
		return ErrProductionInvalid
	}
	s.cancel = cancel
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		if s.isClosed() {
			return
		}
		for {
			// Registration is safe to retry with the installation's exact existing key.
			// Once attached, normal heartbeat owns delivery; no second polling loop.
			if err := s.registerLayerRecipient(run); err == nil {
				return
			}
			if !waitEnvironmentBootstrap(run, 30*time.Second) {
				return
			}
		}
	}()
	return nil
}
func (s *layerEnvironmentService) Shutdown(ctx context.Context) error {
	if ctx == nil || s == nil {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	if s.cancel == nil {
		s.mu.Unlock()
		return ErrProductionInvalid
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	// Cancel before waiting for the service lock. An in-flight Bind may hold the
	// read side of this lock while it is using the service run context; canceling
	// first lets that operation stop instead of making custody cleanup wait on it.
	cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
	}
	return nil
}
func (s *layerEnvironmentService) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
func (s *layerEnvironmentService) EnvironmentForLaunch(ctx context.Context) ([]string, error) {
	return s.layerEnvironmentForLaunch(ctx)
}

func (s *layerEnvironmentService) Environment() ([]string, error) { return nil, envinject.ErrNotReady }
