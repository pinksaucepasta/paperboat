package privatepreviewproxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// PACConfigurator owns only the trusted local PAC setting installed by hostd.
// Recover must restore or reconcile an interrupted prior transaction before a
// new proxy setting is installed. Implementations must never accept network
// PAC discovery or WPAD as Paperboat configuration.
type PACConfigurator interface {
	Recover(context.Context) error
	Install(context.Context, string) error
	Refresh(context.Context, string) error
	Remove(context.Context) error
}

type AccessServiceConfig struct {
	Proxy        AccessProxyConfig
	Configurator PACConfigurator
	// PollInterval bounds discovery latency for newly created private routes.
	PollInterval time.Duration
}

// AccessService owns the local CONNECT listener and its system PAC setting as
// one hostd lifecycle. It starts the listener before publishing the PAC URL,
// and removes the PAC setting before closing the listener.
type AccessService struct {
	config AccessServiceConfig

	lifecycle sync.Mutex
	mu        sync.Mutex
	proxy     *AccessProxy
	running   bool
	cancel    context.CancelFunc
	done      chan struct{}
	pacURL    string
}

func NewAccessService(config AccessServiceConfig) (*AccessService, error) {
	if config.Proxy.Source == nil || config.Configurator == nil {
		return nil, ErrAccessProxyInvalid
	}
	if config.PollInterval == 0 {
		config.PollInterval = 2 * time.Second
	}
	if config.PollInterval < 10*time.Millisecond || config.PollInterval > time.Minute {
		return nil, ErrAccessProxyInvalid
	}
	return &AccessService{config: config}, nil
}

// Start owns the listener and discovery retries. A discovery outage leaves
// system settings untouched; PACURL reports ready only after a valid snapshot
// has been published and installed. Platform installation failures are errors.
func (s *AccessService) Start(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrAccessProxyInvalid
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.proxy != nil {
		return ErrAccessProxyInvalid
	}
	if err := s.config.Configurator.Recover(ctx); err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	proxy, err := StartAccessProxy(ctx, s.config.Proxy)
	if err != nil {
		cancel()
		return err
	}
	discoveryContext, discoveryCancel := context.WithTimeout(runContext, 15*time.Second)
	pacURL, err := proxy.publishPAC(discoveryContext, "")
	discoveryCancel()
	if err != nil {
		// Discovery is an availability dependency, not permission to publish an
		// invented empty route set. Keep the listener unpublished and retry below.
		pacURL = ""
		if ctx.Err() != nil {
			cancel()
			return errors.Join(ctx.Err(), proxy.Close())
		}
	} else if err := s.config.Configurator.Install(ctx, pacURL); err != nil {
		cancel()
		return errors.Join(err, proxy.Close())
	}
	s.proxy = proxy
	s.running = true
	s.cancel = cancel
	s.done = make(chan struct{})
	s.pacURL = pacURL
	go s.monitor(runContext, proxy, s.done)
	return nil
}

func (s *AccessService) Shutdown(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrAccessProxyInvalid
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if !s.running || s.proxy == nil {
		s.mu.Unlock()
		return nil
	}
	proxy := s.proxy
	s.running = false
	s.cancel()
	done := s.done
	s.mu.Unlock()
	// The monitor's network and platform operations share the cancelled run
	// context. Join it before removing settings or closing the listener.
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxy = nil
	return errors.Join(s.config.Configurator.Remove(ctx), proxy.Close())
}

func (s *AccessService) PACURL() (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.proxy == nil || s.pacURL == "" {
		return "", false
	}
	return s.pacURL, true
}

func (s *AccessService) monitor(ctx context.Context, proxy *AccessProxy, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	var pending string
	recoverInstall := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		s.mu.Lock()
		current := s.pacURL
		s.mu.Unlock()
		// Retry the recorded candidate before discovery can retire its PAC body.
		// A failed platform write may have installed this URL on some interfaces.
		next := pending
		var err error
		if next == "" {
			next, err = proxy.publishPAC(attempt, current)
		}
		if err == nil && next != current {
			pending = next
			if current == "" {
				if recoverInstall {
					err = s.config.Configurator.Recover(attempt)
				}
				if err == nil {
					err = s.config.Configurator.Install(attempt, next)
					recoverInstall = err != nil
				}
			} else {
				err = s.config.Configurator.Refresh(attempt, next)
			}
			if err == nil {
				s.mu.Lock()
				s.pacURL = next
				s.mu.Unlock()
				pending = ""
			}
		}
		cancel()
	}
}
