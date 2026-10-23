package inspector

import (
	"context"
	"github.com/pinksaucepasta/paperboat/relay/admission"
	"sync"
	"time"
)

// ReplayBinding is the daemon-observed current authority for one resource. It
// comes from the live forwarding path (latest ingress decision), never from
// client-supplied headers: replay rechecks it before opening the target.
type ReplayBinding struct {
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	ExpiresAt          time.Time
	Forward            ReplayForwarder
	// Available confirms the immutable target snapshot is still installed locally.
	Available func() bool
}

// Registry holds at most one current target-bound replay forwarder per
// resource. Forwarding owners register on activation (or first authorized
// stream) and unregister when the resource stops. Entries are also fenced by
// expiry and local availability. Every replay additionally needs a fresh
// server authorization for the exact generation tuple.
type Registry struct {
	mu      sync.Mutex
	entries map[string]ReplayBinding
	changed chan struct{}
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]ReplayBinding), changed: make(chan struct{})}
}

// Register installs or replaces the current replay binding for a resource.
// Use Unregister to remove an entry.
func (r *Registry) Register(resourceID string, binding ReplayBinding) error {
	if r == nil {
		return ErrInvalid
	}
	if !validResourceID(resourceID) || binding.Forward == nil {
		return ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if previous, ok := r.entries[resourceID]; ok && (binding.ResourceGeneration < previous.ResourceGeneration || binding.RouteGeneration < previous.RouteGeneration || binding.TargetGeneration < previous.TargetGeneration) {
		return ErrStaleGeneration
	}
	if len(r.entries) >= DaemonMaxRecords {
		if _, ok := r.entries[resourceID]; !ok {
			return ErrDropped
		}
	}
	// The just-registered entry may itself already be expired when clocks
	// disagree; keep it so the caller gets an honest stale error rather than
	// a missing-target error.
	r.entries[resourceID] = binding
	r.signalLocked()
	return nil
}

// Unregister removes the current binding, e.g. when forwarding for the
// resource stops. Captured records are unaffected; use Store.Revoke to purge.
func (r *Registry) Unregister(resourceID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, resourceID)
	r.signalLocked()
}

// Current returns the binding when one is registered and unexpired.
func (r *Registry) Current(resourceID string, now time.Time) (ReplayBinding, bool) {
	if r == nil || !validResourceID(resourceID) {
		return ReplayBinding{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.entries[resourceID]
	if !ok || !binding.ExpiresAt.After(now) {
		return ReplayBinding{}, false
	}
	if binding.Available != nil && !binding.Available() {
		return ReplayBinding{}, false
	}
	return binding, true
}

func (r *Registry) signalLocked() {
	if r.changed != nil {
		close(r.changed)
	}
	r.changed = make(chan struct{})
}

// Admit binds a deliberate replay to the registered target generation. Expiry
// rejects new work; withdrawal or replacement cancels already admitted work.
func (r *Registry) Admit(ctx context.Context, resourceID string, binding ReplayBinding) (*admission.Lease, error) {
	lease, err := admission.Admit(ctx, binding.ExpiresAt, nil)
	if err != nil {
		return nil, err
	}
	changes := func() <-chan struct{} { r.mu.Lock(); defer r.mu.Unlock(); return r.changed }
	allowed := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		current, ok := r.entries[resourceID]
		return ok && current.ResourceGeneration == binding.ResourceGeneration && current.RouteGeneration == binding.RouteGeneration && current.TargetGeneration == binding.TargetGeneration && (current.Available == nil || current.Available())
	}
	if err := lease.Watch(changes, allowed); err != nil {
		lease.Close()
		return nil, err
	}
	return lease, nil
}
