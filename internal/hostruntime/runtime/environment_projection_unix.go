//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// projectionEnvironmentService owns only host recipient custody and ciphertext
// delivery. Account passwords and personal/team keys never enter this service.
type projectionEnvironmentService struct {
	mu           sync.RWMutex
	store        *envinject.ProjectionStore
	stateRoot    string
	base         *url.URL
	transport    http.RoundTripper
	registration runtimeidentity.Registration
	credentials  managedSSHIdentitySource
	cancel       context.CancelFunc
	done         chan struct{}
	refresh      sync.Mutex
	closed       bool
}

func newProjectionEnvironmentService(stateRoot string, base *url.URL, transport http.RoundTripper, registration runtimeidentity.Registration, credentials managedSSHIdentitySource) *projectionEnvironmentService {
	return &projectionEnvironmentService{stateRoot: stateRoot, base: base, transport: transport, registration: registration, credentials: credentials, done: make(chan struct{})}
}
func (s *projectionEnvironmentService) Start(ctx context.Context) error {
	if ctx == nil || s == nil || s.credentials == nil {
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
	if err := s.restore(run); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("ENV projection custody needs recovery", "error_code", "environment_projection_state_unavailable")
	}
	go func() {
		defer close(s.done)
		if s.isClosed() {
			return
		}
		for {
			// Registration is safe to retry with the installation's exact existing key.
			// Once attached, normal heartbeat owns delivery; no second polling loop.
			if err := s.ensure(run); err == nil {
				return
			}
			if !waitEnvironmentBootstrap(run, 2*time.Second) {
				return
			}
		}
	}()
	return nil
}
func (s *projectionEnvironmentService) restore(ctx context.Context) error {
	keys, err := productionEnvironmentKeySourceForState(s.stateRoot, s.registration)
	if err != nil {
		return err
	}
	marker, ok := keys.(environmentkey.GenesisMarker)
	if !ok {
		return environmentkey.ErrUnavailable
	}
	material, err := keys.Load(ctx)
	if err != nil {
		return err
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		return err
	}
	store, err := envinject.OpenStoredProjection(ctx, envinject.ProjectionConfig{Path: filepath.Join(s.stateRoot, "environment", "projection.json"), HighWaterPath: filepath.Join(s.stateRoot, "environment-projection-high-water.json"), Issuer: strings.TrimRight(s.base.String(), "/"), MachineID: s.registration.MachineID, InstallationGeneration: uint64(s.registration.InstallationGeneration), HostKeyGeneration: material.Generation, HostPublic: public[:], Keys: keys, GenesisMarker: marker})
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		store.Close()
		return ErrProductionInvalid
	}
	s.store = store
	s.mu.Unlock()
	return nil
}

func (s *projectionEnvironmentService) Shutdown(ctx context.Context) error {
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
	s.mu.Lock()
	store := s.store
	s.store = nil
	s.mu.Unlock()
	if store != nil {
		store.Close()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
	}
	return nil
}
func (s *projectionEnvironmentService) current() *envinject.ProjectionStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil
	}
	return s.store
}
func (s *projectionEnvironmentService) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}
func (s *projectionEnvironmentService) Environment() ([]string, error) {
	if store := s.current(); store != nil {
		return store.Environment()
	}
	return nil, envinject.ErrNotReady
}
func (s *projectionEnvironmentService) BindingState() envinject.BindingState {
	if store := s.current(); store != nil {
		return store.BindingState()
	}
	return envinject.BindingUnknown
}
func (s *projectionEnvironmentService) NextObservation(now time.Time) (envinject.ProjectionObservation, error) {
	if store := s.current(); store != nil {
		return store.NextObservation(now)
	}
	return envinject.ProjectionObservation{}, envinject.ErrNotReady
}
func (s *projectionEnvironmentService) Apply(ctx context.Context, b envinject.ProjectionBundle) error {
	store := s.current()
	if store == nil {
		return envinject.ErrNotReady
	}
	if err := store.Apply(ctx, b); err != nil {
		if !errors.Is(err, envinject.ErrProjectionWriterChanged) {
			return err
		}
		// An explicit personal reset/reprovision may have changed the pinned writer.
		// Reauthenticate the binding through the machine-control registration path.
		if refreshErr := s.ensure(ctx); refreshErr != nil {
			return errors.Join(err, refreshErr)
		}
	}
	return nil
}
func (s *projectionEnvironmentService) ensure(ctx context.Context) error {
	if ctx == nil || s == nil || s.isClosed() {
		return envinject.ErrNotReady
	}
	s.refresh.Lock()
	defer s.refresh.Unlock()
	if s.isClosed() {
		return envinject.ErrNotReady
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	keys, err := productionEnvironmentKeySourceForState(s.stateRoot, s.registration)
	if err != nil {
		return err
	}
	marker, ok := keys.(environmentkey.GenesisMarker)
	if !ok {
		return environmentkey.ErrUnavailable
	}
	material, err := keys.Load(ctx)
	if err != nil {
		return err
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	operationID := "envhost_" + hex.EncodeToString(random[:])
	body, err := json.Marshal(struct {
		OperationID            string `json:"operation_id"`
		InstallationGeneration uint64 `json:"installation_generation"`
		HostKeyGeneration      uint64 `json:"host_key_generation"`
		HostPublic             string `json:"host_public"`
	}{operationID, uint64(s.registration.InstallationGeneration), material.Generation, base64.RawURLEncoding.EncodeToString(public[:])})
	if err != nil {
		return err
	}
	path := "/v1/environment/hosts/" + url.PathEscape(s.registration.MachineID) + "/key"
	token, err := s.credentials.Token(ctx)
	if err != nil {
		return err
	}
	proof, err := s.credentials.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base.ResolveReference(&url.URL{Path: path}).String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Idempotency-Key", operationID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	client := &http.Client{Transport: s.transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrProductionInvalid }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if s.isClosed() {
		return envinject.ErrNotReady
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, 8192)
		return envinject.ErrNotReady
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return envinject.ErrInvalidSnapshot
	}
	bundle, err := envinject.DecodeProjectionResponse(raw, "")
	if err != nil || bundle == nil {
		return envinject.ErrInvalidSnapshot
	}
	if bundle.MachineID != s.registration.MachineID || bundle.InstallationGeneration != uint64(s.registration.InstallationGeneration) || bundle.HostKeyGeneration != material.Generation || bundle.HostPublic != base64.RawURLEncoding.EncodeToString(public[:]) {
		return envinject.ErrInvalidSnapshot
	}
	writer, err := base64.RawURLEncoding.Strict().DecodeString(bundle.WriterPublic)
	if err != nil || len(writer) != 32 {
		return envinject.ErrInvalidSnapshot
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return envinject.ErrNotReady
	}
	store := s.store
	if store != nil {
		err := store.Bind(ctx, *bundle)
		s.mu.RUnlock()
		return err
	}
	store, err = envinject.OpenProjection(ctx, envinject.ProjectionConfig{Path: filepath.Join(s.stateRoot, "environment", "projection.json"), HighWaterPath: filepath.Join(s.stateRoot, "environment-projection-high-water.json"), Issuer: strings.TrimRight(s.base.String(), "/"), AccountID: bundle.AccountID, MachineID: bundle.MachineID, InstallationGeneration: bundle.InstallationGeneration, HostKeyGeneration: bundle.HostKeyGeneration, HostPublic: public[:], WriterPublic: writer, Keys: keys, GenesisMarker: marker})
	if err != nil {
		s.mu.RUnlock()
		return err
	}
	if err := store.Bind(ctx, *bundle); err != nil {
		s.mu.RUnlock()
		store.Close()
		return err
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		store.Close()
		return envinject.ErrNotReady
	}
	if s.store != nil {
		store.Close()
		return s.store.Bind(ctx, *bundle)
	}
	s.store = store
	return nil
}
