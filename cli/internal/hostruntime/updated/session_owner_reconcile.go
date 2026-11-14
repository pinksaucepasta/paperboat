package updated

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// controlMutex serializes owner catalog mutations. The background owner pass
// only borrows it: Lock cancels a pass in progress, which stops at its next
// owner call and retries on the next tick (interrupted retirement is
// recoverable, Coordinator.Ensure/Reconcile). Owner start and install never
// wait behind old owners that will not retire.
type controlMutex struct {
	sync.Mutex
	pass    sync.Mutex
	waiting int
	yield   context.CancelFunc
}

func (m *controlMutex) Lock() {
	m.pass.Lock()
	m.waiting++
	if m.yield != nil {
		m.yield()
	}
	m.pass.Unlock()
	m.Mutex.Lock()
	m.pass.Lock()
	m.waiting--
	m.pass.Unlock()
}

// borrow takes the lock for a background pass unless a control request is
// waiting for it. Release with returnBorrowed.
func (m *controlMutex) borrow(ctx context.Context) (context.Context, bool) {
	if !m.TryLock() {
		return nil, false
	}
	m.pass.Lock()
	defer m.pass.Unlock()
	if m.waiting != 0 {
		m.Unlock()
		return nil, false
	}
	ctx, m.yield = context.WithCancel(ctx)
	return ctx, true
}

func (m *controlMutex) returnBorrowed() {
	m.pass.Lock()
	m.yield()
	m.yield = nil
	m.pass.Unlock()
	m.Unlock()
}

// startSessionOwnerReconcile starts the updater-owned retirement poller. Each
// Owner catalog mutations share the control mutex with authenticated starts.
// Carrier reconciliation owns its native activation lock separately: draining
// a hostd operation must not block the owner-start RPC that operation needs.
func startSessionOwnerReconcile(ctx context.Context, factory func(context.Context) (sessionowner.Coordinator, error), reconcileCarrier func(context.Context) error, controlMu *controlMutex) func() {
	if ctx == nil || (factory == nil && reconcileCarrier == nil) || controlMu == nil {
		return func() {}
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(sessionOwnerReconcileInterval)
		defer ticker.Stop()
		reconcile := func() {
			if workerCtx.Err() != nil {
				return
			}
			passCtx, finish := context.WithTimeout(workerCtx, 2*time.Minute)
			ownerCtx, borrowed := controlMu.borrow(passCtx)
			if !borrowed {
				finish()
				return
			}
			var err error
			if factory != nil {
				var coordinator sessionowner.Coordinator
				coordinator, err = factory(ownerCtx)
				if err == nil {
					err = coordinator.Reconcile(ownerCtx)
				}
			}
			yielded := ownerCtx.Err() != nil && passCtx.Err() == nil
			controlMu.returnBorrowed()
			if yielded {
				err = nil // a control request took the lock; the next tick retries
			}
			if reconcileCarrier != nil && passCtx.Err() == nil {
				err = errors.Join(err, reconcileCarrier(passCtx))
			}
			finish()
			if err != nil && workerCtx.Err() == nil {
				if reporter := errorreport.Current(); reporter != nil {
					reporter.ObserveFailure(workerCtx, "paperboatd", "service", "reconciliation", "service_failed", err)
				}
			}
		}
		reconcile()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				reconcile()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
