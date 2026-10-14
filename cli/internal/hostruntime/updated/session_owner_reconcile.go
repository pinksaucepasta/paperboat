package updated

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

const sessionOwnerReconcileInterval = 5 * time.Second

// startSessionOwnerReconcile starts the updater-owned retirement poller. Each
// Owner catalog mutations share the control mutex with authenticated starts.
// Carrier reconciliation owns its native activation lock separately: draining
// a hostd operation must not block the owner-start RPC that operation needs.
func startSessionOwnerReconcile(ctx context.Context, factory func(context.Context) (sessionowner.Coordinator, error), reconcileCarrier func(context.Context) error, controlMu *sync.Mutex) func() {
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
			if !controlMu.TryLock() {
				return
			}
			passCtx, finish := context.WithTimeout(workerCtx, 2*time.Minute)
			var err error
			if factory != nil {
				var coordinator sessionowner.Coordinator
				coordinator, err = factory(passCtx)
				if err == nil {
					err = coordinator.Reconcile(passCtx)
				}
			}
			controlMu.Unlock()
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
