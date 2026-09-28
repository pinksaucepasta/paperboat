//go:build darwin || linux || windows

package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/networkmonitor"
)

type networkObserver interface {
	Start() error
	Close() error
}

type networkChangeService struct {
	observer networkObserver
	mu       sync.Mutex
	started  bool
	closed   bool
}

type canonicalNetworkRecovery interface{ HandleNetworkEvent(networkmonitor.Event) }

type networkMetricRecorder interface {
	Record(string, float64, map[string]string) error
}

type networkChangeHandler struct {
	connector canonicalNetworkRecovery
	metrics   networkMetricRecorder
}

// SetCanonical attaches the durable connector-v1 recovery path after the
// tunnel assembly is constructed. Production uses only this canonical path.
func (h *networkChangeHandler) SetCanonical(recovery canonicalNetworkRecovery) {
	if h != nil {
		h.connector = recovery
	}
}

func newNetworkChangeHandler(metrics networkMetricRecorder) (*networkChangeHandler, error) {
	if metrics == nil {
		return nil, ErrProductionInvalid
	}
	return &networkChangeHandler{metrics: metrics}, nil
}

func (h *networkChangeHandler) Handle(event networkmonitor.Event) {
	if h == nil || h.metrics == nil || event.Generation == 0 {
		return
	}
	h.record(event)
	if h.connector != nil {
		h.connector.HandleNetworkEvent(event)
	}
}

func (h *networkChangeHandler) record(event networkmonitor.Event) {
	_ = h.metrics.Record("paperboat_runtime_network_generation", float64(event.Generation), nil)
	action := "observe"
	if event.Rebind {
		action = "rebind"
	}
	for _, item := range [...]struct {
		reason networkmonitor.Reason
		label  string
	}{
		{networkmonitor.ReasonDefaultRoute, "default_route"},
		{networkmonitor.ReasonInterfaceAddress, "interface_address"},
		{networkmonitor.ReasonAddressFamily, "address_family"},
		{networkmonitor.ReasonProxy, "proxy"},
		{networkmonitor.ReasonNetworkCost, "network_cost"},
		{networkmonitor.ReasonViability, "viability"},
		{networkmonitor.ReasonWake, "wake"},
		{networkmonitor.ReasonDNS, "dns"},
	} {
		if event.Reasons&item.reason != 0 {
			_ = h.metrics.Record("paperboat_runtime_network_changes_total", 1, map[string]string{"reason": item.label, "action": action})
		}
	}
}

func newNetworkChangeService(changed func(networkmonitor.Event)) (*networkChangeService, error) {
	if changed == nil {
		return nil, ErrProductionInvalid
	}
	observer, err := networkmonitor.New(changed)
	if err != nil {
		return nil, err
	}
	if err := observer.ConfigureDNS(networkmonitor.SystemDNSFingerprint, 15*time.Second); err != nil {
		_ = observer.Close()
		return nil, err
	}
	return &networkChangeService{observer: observer}, nil
}

func newFingerprintingNetworkChangeService(secret []byte, changed func(networkmonitor.Event)) (*networkChangeService, error) {
	if changed == nil || len(secret) < 32 {
		return nil, ErrProductionInvalid
	}
	observer, err := networkmonitor.NewFingerprinting(secret, nil, changed)
	if err != nil {
		return nil, err
	}
	if err := observer.ConfigureDNS(networkmonitor.SystemDNSFingerprint, 15*time.Second); err != nil {
		_ = observer.Close()
		return nil, err
	}
	return &networkChangeService{observer: observer}, nil
}

func (s *networkChangeService) Start(context.Context) error {
	if s == nil || s.observer == nil {
		return ErrProductionInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return ErrProductionInvalid
	}
	if err := s.observer.Start(); err != nil {
		return err
	}
	s.started = true
	return nil
}

func (s *networkChangeService) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	observer := s.observer
	s.mu.Unlock()
	if observer == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- observer.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
