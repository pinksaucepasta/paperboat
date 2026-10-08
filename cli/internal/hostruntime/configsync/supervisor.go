package configsync

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrSupervisorInvalid = errors.New("invalid config sync supervisor")
	ErrCredentialInvalid = errors.New("invalid config sync credential")
)

type SupervisorConfig struct {
	Credentials        CredentialSource
	Factory            RuntimeFactory
	Retry              time.Duration
	ConfigRevision     func(context.Context) (string, error)
	ConfigPollInterval time.Duration
}

// Supervisor keeps config sync disabled while authorization is unavailable.
// It is safe to run as a required helper component: lack of
// assignment or current consent does not prevent unrelated helper operation,
// and no runtime (therefore no managed filesystem access) exists before a
// fresh credential is returned.
type Supervisor struct {
	credentials        CredentialSource
	factory            RuntimeFactory
	retry              time.Duration
	configRevision     func(context.Context) (string, error)
	configPollInterval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
	active Runtime
}

func NewSupervisor(config SupervisorConfig) (*Supervisor, error) {
	if config.Credentials == nil || config.Factory == nil {
		return nil, ErrSupervisorInvalid
	}
	if config.Retry == 0 {
		config.Retry = 30 * time.Second
	}
	if config.Retry < time.Second || config.Retry > 5*time.Minute {
		return nil, ErrSupervisorInvalid
	}
	if config.ConfigPollInterval == 0 {
		config.ConfigPollInterval = time.Second
	}
	if config.ConfigPollInterval < 100*time.Millisecond || config.ConfigPollInterval > time.Minute {
		return nil, ErrSupervisorInvalid
	}
	return &Supervisor{configRevision: config.ConfigRevision, configPollInterval: config.ConfigPollInterval, credentials: config.Credentials, factory: config.Factory, retry: config.Retry}, nil
}

func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	done := make(chan error, 1)
	s.done = done
	go func() {
		done <- s.run(runCtx)
		close(done)
	}()
	return nil
}

func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done, s.active = nil, nil, nil
	s.mu.Unlock()
	s.credentials.InvalidateCredential()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var failures configSyncFailureObservation
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		configurationRevision := ""
		if s.configRevision != nil {
			var configErr error
			configurationRevision, configErr = s.configRevision(ctx)
			if configErr != nil {
				failures.observe(ctx, "reconciliation", configErr)
				if !resetSupervisorTimer(ctx, timer, s.retry) {
					return nil
				}
				continue
			}
		}
		credential, err := s.credentials.Credential(ctx)
		if err != nil || !validActivationCredential(credential) {
			if err != nil {
				failures.observe(ctx, "control_request", err)
			} else {
				failures.observe(ctx, "control_request", ErrCredentialInvalid)
			}
			s.credentials.InvalidateCredential()
			if !resetSupervisorTimer(ctx, timer, s.retry) {
				return nil
			}
			continue
		}
		activeCtx, cancelActive := context.WithCancel(ctx)
		runtime, err := s.factory(activeCtx, credential)
		if err != nil || runtime == nil {
			if err == nil {
				err = ErrSupervisorInvalid
			}
			failures.observe(ctx, "component_start", err)
			cancelActive()
			s.credentials.InvalidateCredential()
			if !resetSupervisorTimer(ctx, timer, s.retry) {
				return nil
			}
			continue
		}
		if err := runtime.Start(activeCtx); err != nil {
			cancelActive()
			shutdownErr := runtime.Shutdown(WithoutFinalFlush(context.WithoutCancel(ctx)))
			failures.observe(ctx, "component_start", errors.Join(err, shutdownErr))
			s.credentials.InvalidateCredential()
			if !resetSupervisorTimer(ctx, timer, s.retry) {
				return nil
			}
			continue
		}
		failures.recovered(ctx)
		s.mu.Lock()
		if s.cancel != nil {
			s.active = runtime
		}
		s.mu.Unlock()
		configChanged := make(chan error, 1)
		monitorDone := make(chan struct{})
		go func() {
			defer close(monitorDone)
			if s.configRevision == nil {
				return
			}
			ticker := time.NewTicker(s.configPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-activeCtx.Done():
					return
				case <-ticker.C:
					revision, err := s.configRevision(activeCtx)
					if err != nil || revision != configurationRevision {
						if err == nil {
							err = ErrConfigurationChanged
						} else {
							failures.observe(activeCtx, "reconciliation", err)
						}
						configChanged <- err
						cancelActive()
						return
					}
				}
			}
		}()
		revalidate := time.NewTicker(s.retry)
		authorized := true
		var configurationErr error
		for authorized {
			select {
			case <-ctx.Done():
				authorized = false
			case configurationErr = <-configChanged:
				authorized = false
			case <-revalidate.C:
				revalidateCredential(s.credentials)
				current, credentialErr := s.credentials.Credential(activeCtx)
				authorized = credentialErr == nil && sameCredentialBinding(credential, current)
				if credentialErr != nil {
					failures.observe(activeCtx, "control_request", credentialErr)
				} else if !validActivationCredential(current) {
					failures.observe(activeCtx, "control_request", ErrCredentialInvalid)
				}
			}
		}
		revalidate.Stop()
		cancelActive()
		<-monitorDone
		if configurationErr == nil {
			select {
			case configurationErr = <-configChanged:
			default:
			}
		}
		s.mu.Lock()
		if s.active == runtime {
			s.active = nil
		}
		s.mu.Unlock()
		if ctx.Err() == nil && configurationErr == nil {
			s.credentials.InvalidateCredential()
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(WithoutFinalFlush(context.WithoutCancel(ctx)), 30*time.Second)
		shutdownErr := runtime.Shutdown(shutdownCtx)
		if ctx.Err() == nil && shutdownErr != nil {
			failures.observe(ctx, "component_shutdown", shutdownErr)
		}
		if ctx.Err() == nil && configurationErr != nil {
			if reporter, ok := runtime.(interface{ PauseConfiguration(context.Context, error) }); ok {
				reporter.PauseConfiguration(shutdownCtx, configurationErr)
			}
			s.credentials.InvalidateCredential()
		}
		shutdownCancel()
		if ctx.Err() != nil {
			return shutdownErr
		}
		if !resetSupervisorTimer(ctx, timer, s.retry) {
			return nil
		}
	}
}

func revalidateCredential(credentials CredentialSource) {
	if revalidator, ok := credentials.(interface{ RevalidateCredential() }); ok {
		revalidator.RevalidateCredential()
		return
	}
	credentials.InvalidateCredential()
}

func validActivationCredential(credential Credential) bool {
	return credential.Value != "" && credential.EnvironmentID != "" && credential.MachineID != "" &&
		credential.AssignmentID != "" && credential.WarningRevision != "" && !credential.ExpiresAt.IsZero()
}

func sameCredentialBinding(expected, current Credential) bool {
	return validActivationCredential(current) &&
		expected.EnvironmentID == current.EnvironmentID &&
		expected.MachineID == current.MachineID &&
		expected.AssignmentID == current.AssignmentID &&
		expected.AssignmentVersion == current.AssignmentVersion &&
		expected.WarningRevision == current.WarningRevision
}

func resetSupervisorTimer(ctx context.Context, timer *time.Timer, duration time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	timer.Reset(duration)
	return true
}

type noFinalFlushKey struct{}

func WithoutFinalFlush(ctx context.Context) context.Context {
	return context.WithValue(ctx, noFinalFlushKey{}, true)
}
func FinalFlushAllowed(ctx context.Context) bool {
	disabled, _ := ctx.Value(noFinalFlushKey{}).(bool)
	return !disabled
}
