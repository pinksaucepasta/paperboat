package configsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type supervisorCredentials struct {
	mu                 sync.Mutex
	credential         Credential
	err                error
	calls              chan struct{}
	invalid            int
	revalidated        int
	revokeOnRevalidate bool
}

func (s *supervisorCredentials) Credential(context.Context) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.calls <- struct{}{}:
	default:
	}
	return s.credential, s.err
}

func (s *supervisorCredentials) InvalidateCredential() {
	s.mu.Lock()
	s.invalid++
	s.mu.Unlock()
}

func (s *supervisorCredentials) RevalidateCredential() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revalidated++
	if s.revokeOnRevalidate {
		s.err = ErrAuthorization
		s.credential = Credential{}
	}
}

type supervisorRuntime struct {
	started  chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	stopErr  error
}

func (r *supervisorRuntime) Start(context.Context) error {
	close(r.started)
	return nil
}

func (r *supervisorRuntime) Shutdown(context.Context) error {
	r.stopOnce.Do(func() { close(r.stopped) })
	return r.stopErr
}

func TestSupervisorDoesNotConstructRuntimeBeforeEligibility(t *testing.T) {
	credentials := &supervisorCredentials{err: ErrAuthorization, calls: make(chan struct{}, 4)}
	constructed := make(chan *supervisorRuntime, 1)
	supervisor, err := NewSupervisor(SupervisorConfig{
		Credentials: credentials, Retry: time.Second,
		Factory: func(context.Context, Credential) (Runtime, error) {
			runtime := &supervisorRuntime{started: make(chan struct{}), stopped: make(chan struct{})}
			constructed <- runtime
			return runtime, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-credentials.calls:
	case <-time.After(time.Second):
		t.Fatal("authorization was not checked")
	}
	select {
	case <-constructed:
		t.Fatal("runtime constructed without eligibility")
	default:
	}

	credentials.mu.Lock()
	credentials.err = nil
	credentials.credential = Credential{
		Value: "credential", EnvironmentID: "environment", MachineID: "helper",
		AssignmentID: "assignment", WarningRevision: "warning", ExpiresAt: time.Now().Add(time.Minute),
	}
	credentials.mu.Unlock()
	select {
	case runtime := <-constructed:
		select {
		case <-runtime.started:
		case <-time.After(time.Second):
			t.Fatal("eligible runtime was not started")
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		if err := supervisor.Shutdown(shutdownCtx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-runtime.stopped:
		default:
			t.Fatal("runtime was not stopped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("eligible runtime was not constructed")
	}
}

func TestSupervisorObservesAndRecoversFromInvalidCredentialWithoutError(t *testing.T) {
	credentials := &supervisorCredentials{calls: make(chan struct{}, 8)}
	runtime := &supervisorRuntime{started: make(chan struct{}), stopped: make(chan struct{})}
	faults := make(chan errorreport.Fault, 1)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		select {
		case faults <- fault:
		default:
		}
	})
	defer restore()

	reference := supportref.New()
	recorder := diagnostics.NewMemoryRecorder()
	ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), recorder)
	supervisor, err := NewSupervisor(SupervisorConfig{
		Credentials: credentials, Retry: time.Second,
		Factory: func(context.Context, Credential) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-credentials.calls:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not check the incomplete credential")
	}
	var fault errorreport.Fault
	select {
	case fault = <-faults:
	case <-time.After(time.Second):
		t.Fatal("incomplete nil-error credential was not observed")
	}
	if fault.Code != "config_sync_failed" || fault.Stage != "control_request" || fault.SupportReference != reference {
		t.Fatalf("invalid credential fault = %#v", fault)
	}

	credentials.mu.Lock()
	credentials.credential = Credential{
		Value: "credential", EnvironmentID: "environment", MachineID: "helper",
		AssignmentID: "assignment", WarningRevision: "warning", ExpiresAt: time.Now().Add(time.Minute),
	}
	credentials.mu.Unlock()
	select {
	case <-runtime.started:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not activate after the credential became valid")
	}
	var recovered bool
	for _, event := range recorder.Recent() {
		if event.Code == "recovered" {
			recovered = event.SupportReference == reference
		}
	}
	if !recovered {
		t.Fatal("credential recovery was not recorded with the original support reference")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorValidatesConfiguration(t *testing.T) {
	if _, err := NewSupervisor(SupervisorConfig{}); !errors.Is(err, ErrSupervisorInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestSupervisorStopsRuntimeAndClearsAuthorizationAfterRevocation(t *testing.T) {
	credentials := &supervisorCredentials{
		calls: make(chan struct{}, 8),
		credential: Credential{
			Value: "credential", EnvironmentID: "environment", MachineID: "helper",
			AssignmentID: "assignment", WarningRevision: "warning", ExpiresAt: time.Now().Add(time.Minute),
		},
		revokeOnRevalidate: true,
	}
	runtime := &supervisorRuntime{started: make(chan struct{}), stopped: make(chan struct{})}
	factoryCalls := 0
	supervisor, err := NewSupervisor(SupervisorConfig{
		Credentials: credentials, Retry: time.Second,
		Factory: func(context.Context, Credential) (Runtime, error) {
			factoryCalls++
			return runtime, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("eligible runtime did not start")
	}
	select {
	case <-runtime.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not stop after authorization revocation")
	}
	credentials.mu.Lock()
	revalidated, invalidated := credentials.revalidated, credentials.invalid
	credentials.mu.Unlock()
	if revalidated != 1 || invalidated == 0 || factoryCalls != 1 {
		t.Fatalf("revalidated=%d invalidated=%d factory_calls=%d", revalidated, invalidated, factoryCalls)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := supervisor.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorReturnsFinalFlushFailure(t *testing.T) {
	credentials := &supervisorCredentials{
		calls: make(chan struct{}, 4),
		credential: Credential{
			Value: "credential", EnvironmentID: "environment", MachineID: "helper",
			AssignmentID: "assignment", WarningRevision: "warning", ExpiresAt: time.Now().Add(time.Minute),
		},
	}
	flushErr := errors.New("flush failed")
	runtime := &supervisorRuntime{
		started: make(chan struct{}), stopped: make(chan struct{}), stopErr: flushErr,
	}
	supervisor, err := NewSupervisor(SupervisorConfig{
		Credentials: credentials, Retry: time.Second,
		Factory: func(context.Context, Credential) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("runtime did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); !errors.Is(err, flushErr) {
		t.Fatalf("shutdown error = %v", err)
	}
}

type configMonitoredRuntime struct {
	started, cancelled chan struct{}
	stopped            chan bool
}

func (r *configMonitoredRuntime) Start(ctx context.Context) error {
	close(r.started)
	go func() { <-ctx.Done(); close(r.cancelled) }()
	return nil
}
func (r *configMonitoredRuntime) Shutdown(ctx context.Context) error {
	select {
	case <-r.cancelled:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.stopped <- FinalFlushAllowed(ctx)
	return nil
}

type configPauseStatusSink struct {
	credentials *supervisorCredentials
	paused      chan Status
	err         chan error
}

func (s configPauseStatusSink) ReportStatus(ctx context.Context, status Status, _ int) error {
	if status.ErrorCode != "configuration_changed" && status.ErrorCode != "configuration_invalid" {
		return nil
	}
	s.credentials.mu.Lock()
	invalid := s.credentials.invalid
	s.credentials.mu.Unlock()
	if ctx.Err() != nil || invalid != 0 {
		s.err <- errors.New("pause status reported after cancellation or credential invalidation")
		return ErrAuthorization
	}
	s.paused <- status
	return nil
}

type configPauseEngine struct {
	*Engine
	started chan struct{}
}

func (e configPauseEngine) Start(ctx context.Context) error {
	if err := e.Engine.Start(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.status.ManagedPathCount, e.status.LastAppliedRevision = 2, "approved-head"
	e.mu.Unlock()
	close(e.started)
	return nil
}

func TestSupervisorFileEditImmediatelyReportsActionablePauseBeforeInvalidation(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "invalid"}[invalid], func(t *testing.T) {
			credentials := &supervisorCredentials{calls: make(chan struct{}, 8), credential: Credential{Value: "token", EnvironmentID: "environment", MachineID: "helper", AssignmentID: "assignment", WarningRevision: "warning", ExpiresAt: time.Now().Add(time.Hour)}}
			sink := configPauseStatusSink{credentials: credentials, paused: make(chan Status, 1), err: make(chan error, 1)}
			syncer := &recordingSyncer{results: make(chan struct{}, 8)}
			statusPath := filepath.Join(resolvedTempDir(t), "status.json")
			engine, err := NewEngine(EngineConfig{HomeRoot: resolvedTempDir(t), Descriptor: testRuntimeDescriptor(), Syncer: syncer, Statuses: sink, StatusPath: statusPath})
			if err != nil {
				t.Fatal(err)
			}
			runtime := configPauseEngine{Engine: engine, started: make(chan struct{})}
			var revisionMu sync.Mutex
			changed := false
			revision := func(context.Context) (string, error) {
				revisionMu.Lock()
				defer revisionMu.Unlock()
				if changed && invalid {
					return "", ErrSourceConfigInvalid
				}
				if changed {
					return "edited", nil
				}
				return "approved", nil
			}
			supervisor, err := NewSupervisor(SupervisorConfig{Credentials: credentials, ConfigRevision: revision, ConfigPollInterval: 100 * time.Millisecond, Retry: time.Minute, Factory: func(context.Context, Credential) (Runtime, error) { return runtime, nil }})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := supervisor.Start(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-runtime.started:
			case <-time.After(time.Second):
				t.Fatal("engine did not start")
			}
			revisionMu.Lock()
			changed = true
			revisionMu.Unlock()
			var paused Status
			select {
			case paused = <-sink.paused:
			case err := <-sink.err:
				t.Fatal(err)
			case <-time.After(time.Second):
				t.Fatal("pause action waited for factory retry")
			}
			wantCode, wantAction := "configuration_changed", "apply_configuration"
			if invalid {
				wantCode, wantAction = "configuration_invalid", "fix_configuration"
			}
			if paused.ErrorCode != wantCode || len(paused.RecoveryActions) != 1 || paused.RecoveryActions[0] != wantAction || paused.ManagedPathCount != 2 || paused.LastAppliedRevision != "approved-head" || paused.RemoteRevision != "head" || paused.SyncRevision != 2 {
				t.Fatalf("pause lost action or reconciled state: %#v", paused)
			}
			persisted, err := ReadStatus(statusPath, engine.descriptor.Policy.SummaryLimit)
			if err != nil || persisted.ErrorCode != wantCode {
				t.Fatalf("local pause status %#v %v", persisted, err)
			}
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
			defer shutdownCancel()
			if err := supervisor.Shutdown(shutdownCtx); err != nil {
				t.Fatal(err)
			}
			syncer.mu.Lock()
			calls := syncer.calls
			syncer.mu.Unlock()
			if calls != 1 {
				t.Fatalf("file edit flushed old configuration: %d sync calls", calls)
			}
		})
	}
}

func TestSupervisorMachineFileChangesStopOldRuntimeWithoutFlushOffline(t *testing.T) {
	for _, change := range []string{"replace", "invalid", "remove"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(resolvedTempDir(t), "config-sync.toml")
			if err := os.WriteFile(path, []byte(""), 0600); err != nil {
				t.Fatal(err)
			}
			credentials := &supervisorCredentials{credential: Credential{Value: "token", EnvironmentID: "env", MachineID: "machine", AssignmentID: "assignment", WarningRevision: "warning", ExpiresAt: time.Now().Add(time.Hour)}, calls: make(chan struct{}, 8)}
			runtime := &configMonitoredRuntime{started: make(chan struct{}), cancelled: make(chan struct{}), stopped: make(chan bool, 1)}
			revision := func(ctx context.Context) (string, error) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if _, err := LoadSourceConfig(path, DefaultSourceConfigLimits()); err != nil {
					return "", err
				}
				data, err := os.ReadFile(path)
				if errors.Is(err, os.ErrNotExist) {
					return "missing", nil
				}
				if err != nil {
					return "", err
				}
				return hashSnapshotBytes(data), nil
			}
			supervisor, err := NewSupervisor(SupervisorConfig{Credentials: credentials, Retry: time.Second, ConfigPollInterval: 100 * time.Millisecond, ConfigRevision: revision, Factory: func(context.Context, Credential) (Runtime, error) { return runtime, nil }})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := supervisor.Start(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-runtime.started:
			case <-time.After(time.Second):
				t.Fatal("runtime not started")
			}
			credentials.mu.Lock()
			credentials.err = ErrRepositoryUnavailable
			credentials.mu.Unlock()
			switch change {
			case "remove":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "replace":
				next := path + ".new"
				if err := os.WriteFile(next, []byte(`automatic_updates = true`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(next, path); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case flushAllowed := <-runtime.stopped:
				if flushAllowed {
					t.Fatal("stale runtime final flush allowed")
				}
			case <-time.After(time.Second):
				t.Fatal("machine edit did not stop offline runtime")
			}
			if err := supervisor.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
