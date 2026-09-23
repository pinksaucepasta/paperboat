package diagnostics

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"sync"
)

const MemoryCapacity = 512

type MemoryRing struct {
	mu     sync.RWMutex
	events [MemoryCapacity]Event
	next   uint64
	count  int
}

func (r *MemoryRing) Record(event Event) error {
	if r == nil || event.Validate() != nil {
		return ErrInvalid
	}
	r.mu.Lock()
	r.events[r.next%MemoryCapacity] = cloneEvent(event)
	r.next++
	if r.count < MemoryCapacity {
		r.count++
	}
	r.mu.Unlock()
	outcome := "state_change"
	if event.Severity == "error" {
		outcome = "failed"
	}
	errorreport.Current().Observe(supportref.WithContext(context.Background(), event.SupportReference), "paperboat-daemon", "diagnostic", outcome, -1)
	return nil
}

func (r *MemoryRing) Snapshot() []Event {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Event, 0, r.count)
	start := r.next - uint64(r.count)
	for index := 0; index < r.count; index++ {
		result = append(result, cloneEvent(r.events[(start+uint64(index))%MemoryCapacity]))
	}
	return result
}
