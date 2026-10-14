package managedssh

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var (
	ErrSSHHostInvalid = errors.New("managed SSH host service is invalid")
	ErrSSHHostStale   = errors.New("managed SSH host target generation is stale")
	ErrSSHHostBusy    = errors.New("managed SSH host stream capacity is exhausted")
)

type HostTarget struct {
	Generation   uint64
	SourceSHA256 string
	Port         uint16
	Readiness    SSHReadiness
}

type HostConfig struct {
	MaxStreams   int
	ProbeTimeout time.Duration
	DialTimeout  time.Duration
}

// Host owns the bounded runtime boundary between an authorized Paperboat SSH
// stream and the machine's existing loopback sshd.
type Host struct {
	mu             sync.RWMutex
	config         HostConfig
	target         HostTarget
	capacity       chan struct{}
	active         map[uint64]int
	activeBySource map[string]int
	retired        bool
	disabled       bool
	flows          map[*hostFlow]struct{}
}

type hostFlow struct{ cancel context.CancelFunc }

func NewHost(config HostConfig) (*Host, error) {
	if config.MaxStreams < 1 || config.MaxStreams > 1024 || config.ProbeTimeout <= 0 || config.ProbeTimeout > 30*time.Second || config.DialTimeout <= 0 || config.DialTimeout > 30*time.Second {
		return nil, ErrSSHHostInvalid
	}
	return &Host{config: config, capacity: make(chan struct{}, config.MaxStreams), active: make(map[uint64]int), activeBySource: make(map[string]int), flows: make(map[*hostFlow]struct{})}, nil
}

// ReconcileTarget proves listener readiness for a source-less target. Callers
// that know the installed runtime identity should use ReconcileSourceTarget.
func (h *Host) ReconcileTarget(ctx context.Context, generation uint64, port uint16) (HostTarget, error) {
	return h.ReconcileSourceTarget(ctx, generation, port, "")
}

// ReconcileSourceTarget associates a worker epoch with the immutable runtime
// source whose installed SSH wrapper it can keep using across worker restarts.
// Equal-epoch replay must preserve both the port and the source SHA-256.
func (h *Host) ReconcileSourceTarget(ctx context.Context, generation uint64, port uint16, sourceSHA256 string) (HostTarget, error) {
	if h == nil || ctx == nil || generation == 0 || port == 0 || sourceSHA256 != "" && !validSourceSHA256(sourceSHA256) {
		return HostTarget{}, ErrSSHHostInvalid
	}
	h.mu.RLock()
	current := h.target
	retired := h.retired || h.disabled
	h.mu.RUnlock()
	if retired {
		return HostTarget{}, ErrSSHHostStale
	}
	if current.Generation > generation || current.Generation == generation && (current.Port != port || current.SourceSHA256 != sourceSHA256) {
		return HostTarget{}, ErrSSHHostStale
	}
	if current.Generation == generation {
		return current, nil
	}
	readiness, err := ProbeLoopbackSSH(ctx, port, h.config.ProbeTimeout)
	if err != nil {
		return HostTarget{}, err
	}
	next := HostTarget{Generation: generation, SourceSHA256: sourceSHA256, Port: port, Readiness: readiness}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.retired || h.disabled {
		return HostTarget{}, ErrSSHHostStale
	}
	if h.target.Generation > generation || h.target.Generation == generation && (h.target.Port != port || h.target.SourceSHA256 != sourceSHA256) {
		return HostTarget{}, ErrSSHHostStale
	}
	if h.target.Generation == generation {
		return h.target, nil
	}
	h.target = next
	return next, nil
}

func validSourceSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b < '0' || b > '9' && b < 'a' || b > 'f' {
			return false
		}
	}
	return true
}

func (h *Host) Target() (HostTarget, bool) {
	if h == nil {
		return HostTarget{}, false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.target, h.target.Generation != 0 && !h.retired && !h.disabled
}

// Retire fences new bridges only once every accepted connection has finished.
// A busy result leaves admission open and never interrupts an existing SSH flow.
func (h *Host) Retire() error {
	if h == nil {
		return ErrSSHHostInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.active) != 0 {
		return ErrSSHHostBusy
	}
	h.retired = true
	return nil
}

// ResumeAdmissions restores the same carrier after an aborted native replacement.
func (h *Host) ResumeAdmissions() error {
	if h == nil {
		return ErrSSHHostInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retired = false
	return nil
}

// Disable revokes accepted SSH bridges and waits for their resources to close.
// This explicit capability change is separate from update retirement, which
// must never cancel a user's existing SSH connection.
func (h *Host) Disable(ctx context.Context) error {
	if h == nil || ctx == nil {
		return ErrSSHHostInvalid
	}
	h.mu.Lock()
	h.disabled = true
	for flow := range h.flows {
		flow.cancel()
	}
	h.mu.Unlock()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		h.mu.RLock()
		empty := len(h.flows) == 0
		h.mu.RUnlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Enable restores admission after the current worker applies an enabled policy.
// An in-progress native retirement fence remains closed.
func (h *Host) Enable() error {
	if h == nil {
		return ErrSSHHostInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.disabled = false
	return nil
}

// CheckSourceRetirement proves an old installed source has no accepted SSH
// bridges. A source cannot retire while it is still the current target, even
// when admission is disabled or no streams are active. An unknown current
// source also fails closed because there is no proof that it is noncurrent.
func (h *Host) CheckSourceRetirement(sourceSHA256 string) error {
	if h == nil || !validSourceSHA256(sourceSHA256) {
		return ErrSSHHostInvalid
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.target.SourceSHA256 == "" || h.target.SourceSHA256 == sourceSHA256 {
		return ErrSSHHostStale
	}
	if h.activeBySource[sourceSHA256] != 0 {
		return ErrSSHHostBusy
	}
	return nil
}

func (h *Host) Serve(ctx context.Context, generation uint64, stream io.ReadWriteCloser) (BridgeResult, error) {
	if h == nil || ctx == nil || generation == 0 || stream == nil {
		return BridgeResult{}, ErrSSHHostInvalid
	}
	h.mu.Lock()
	target := h.target
	if h.retired || h.disabled || target.Generation == 0 || target.Generation != generation {
		h.mu.Unlock()
		return BridgeResult{}, ErrSSHHostStale
	}
	select {
	case h.capacity <- struct{}{}:
		h.active[generation]++
		h.activeBySource[target.SourceSHA256]++
	default:
		h.mu.Unlock()
		return BridgeResult{}, ErrSSHHostBusy
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	flow := &hostFlow{cancel: cancel}
	h.flows[flow] = struct{}{}
	h.mu.Unlock()
	defer func() {
		cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.flows, flow)
		h.active[generation]--
		if h.active[generation] == 0 {
			delete(h.active, generation)
		}
		h.activeBySource[target.SourceSHA256]--
		if h.activeBySource[target.SourceSHA256] == 0 {
			delete(h.activeBySource, target.SourceSHA256)
		}
		<-h.capacity
	}()
	return BridgeSSH(bridgeCtx, stream, target.Readiness.Target, h.config.DialTimeout)
}
