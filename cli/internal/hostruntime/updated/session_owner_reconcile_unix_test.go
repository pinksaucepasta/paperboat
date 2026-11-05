//go:build linux || darwin

package updated

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// Carrier maintenance waits for already admitted hostd operations. An operation
// may need the real owner-start control path before it can release admission.
func TestCarrierReconcileAllowsOwnerStartDuringDrainAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admitted := make(chan struct{})
	reachedStart := make(chan struct{})
	carrierExited := make(chan struct{})
	startErr := errors.New("owner start reached protected factory")
	s := &Service{config: Config{SessionOwners: func(context.Context) (sessionowner.Coordinator, error) {
		close(reachedStart)
		return sessionowner.Coordinator{}, startErr
	}}}
	stop := startSessionOwnerReconcile(ctx, nil, func(ctx context.Context) error {
		defer close(carrierExited)
		close(admitted)
		select {
		case <-reachedStart:
		case <-ctx.Done():
			return ctx.Err()
		}
		// Keep maintenance active until cancellation; owner starts must still enter.
		<-ctx.Done()
		return ctx.Err()
	}, &s.controlMu)
	<-admitted
	startDone := make(chan error, 1)
	go func() {
		_, err := s.controlRequestWithRequest(context.Background(), ControlRequest{Schema: ControlProtocolV1, Operation: "session-owner-start", Generation: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		startDone <- err
	}()
	select {
	case err := <-startDone:
		if !errors.Is(err, startErr) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		cancel()
		stop()
		<-startDone
		t.Fatal("carrier drain blocked the admitted operation's owner start")
	}
	cancel()
	stop()
	select {
	case <-carrierExited:
	default:
		t.Fatal("carrier reconcile leaked after cancellation")
	}
}

// A stale owner registration must not prevent the independently authenticated
// carrier from advancing to the installed runtime.
func TestCarrierReconcileRunsAfterOwnerReconcileFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 1)
	var mu controlMutex
	stop := startSessionOwnerReconcile(ctx, func(context.Context) (sessionowner.Coordinator, error) {
		return sessionowner.Coordinator{}, sessionowner.ErrInvalid
	}, func(context.Context) error {
		called <- struct{}{}
		return nil
	}, &mu)
	defer stop()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("owner reconciliation failure prevented independent carrier reconciliation")
	}
}

// dadape: an owner pass waiting on old owners that will not retire held
// controlMu until its 2-minute deadline, delaying the update's owner start.
// The pass yields: a control request that needs the lock cancels it.
func TestOwnerReconcileYieldsControlLockToOwnerStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	passEnded := make(chan error, 1)
	startErr := errors.New("owner start reached protected factory")
	var passes sync.Once
	s := &Service{}
	s.config.SessionOwners = func(ctx context.Context) (sessionowner.Coordinator, error) {
		first := false
		passes.Do(func() { first = true })
		if !first {
			return sessionowner.Coordinator{}, startErr
		}
		// The background pass: an old owner that never confirms retirement.
		close(waiting)
		<-ctx.Done()
		passEnded <- ctx.Err()
		return sessionowner.Coordinator{}, ctx.Err()
	}
	stop := startSessionOwnerReconcile(ctx, s.config.SessionOwners, nil, &s.controlMu)
	defer stop()
	<-waiting
	startDone := make(chan error, 1)
	go func() {
		_, err := s.controlRequestWithRequest(context.Background(), ControlRequest{Schema: ControlProtocolV1, Operation: "session-owner-start", Generation: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		startDone <- err
	}()
	select {
	case err := <-startDone:
		if !errors.Is(err, startErr) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-startDone
		t.Fatal("owner start waited behind the background owner pass")
	}
	select {
	case err := <-passEnded:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("background pass ended with %v, want yield", err)
		}
	default:
		t.Fatal("background pass still holds its wait after yielding the lock")
	}
}
