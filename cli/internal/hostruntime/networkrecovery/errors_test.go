package networkrecovery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type recoveryErrorSpy struct{}

func (recoveryErrorSpy) Error() string { panic("private cause must not be formatted") }

type recoveryCycle struct{}

func (*recoveryCycle) Error() string   { return "PRIVATE_CYCLE" }
func (e *recoveryCycle) Unwrap() error { return e }

type recoveryTypedNil struct{}

func (*recoveryTypedNil) Error() string { return "PRIVATE_NIL" }
func (*recoveryTypedNil) Unwrap() error { panic("typed nil must not be traversed") }

func TestNetworkRecoveryCompleteCauseClassificationAndPrivacy(t *testing.T) {
	var nilCause *recoveryTypedNil
	cases := []struct {
		name      string
		err       error
		class     FailureClass
		permanent bool
	}{
		{"wrapped_cancel", fmt.Errorf("wrapper: %w", context.Canceled), FailureCanceled, true},
		{"duplicate_cancel", errors.Join(context.Canceled, context.Canceled), FailureCanceled, true},
		{"deadline_cancel", errors.Join(context.Canceled, context.DeadlineExceeded), FailureTimeout, false},
		{"mixed_io_cancel", errors.Join(context.Canceled, syscall.EIO), FailureLocalSystem, false},
		{"mixed_auth_io", errors.Join(ErrAuthentication, syscall.EIO), FailureUnknown, false},
		{"wrapped_dns", fmt.Errorf("wrapper: %w", ErrDNS), FailureDNS, false},
		{"auth", ErrAuthentication, FailureAuthentication, true},
		{"configuration", ErrConfiguration, FailureConfiguration, true},
		{"cycle", errors.Join(context.Canceled, &recoveryCycle{}), FailureUnknown, false},
		{"typed_nil", errors.Join(context.Canceled, nilCause), FailureUnknown, false},
		{"marked_dns_errno", Failure{Class: FailureDNS, Cause: syscall.EIO}, FailureDNS, false},
		{"marked_auth_errno", Failure{Class: FailureAuthentication, Permanent: true, Cause: syscall.EIO}, FailureAuthentication, true},
		{"marked_config_deadline", Failure{Class: FailureConfiguration, Permanent: true, Cause: context.DeadlineExceeded}, FailureConfiguration, true},
		{"marked_dns_known_auth", Failure{Class: FailureDNS, Cause: ErrAuthentication}, FailureDNS, false},
		{"first_unary_marker", Failure{Class: FailureDNS, Cause: Failure{Class: FailureConfiguration, Permanent: true, Cause: syscall.EIO}}, FailureDNS, false},
		{"marked_dns_independent_errno", errors.Join(Failure{Class: FailureDNS, Cause: ErrDNS}, syscall.EIO), FailureUnknown, false},
		{"marked_auth_internal_join", Failure{Class: FailureAuthentication, Permanent: true, Cause: errors.Join(ErrAuthentication, syscall.EIO)}, FailureUnknown, false},
		{"marker_nil_cancel", Failure{Class: FailureCanceled, Permanent: true}, FailureUnknown, false},
		{"marker_unknown_cancel", Failure{Class: FailureCanceled, Permanent: true, Cause: recoveryErrorSpy{}}, FailureUnknown, false},
		{"marker_mixed", Failure{Class: FailureCanceled, Permanent: true, Cause: errors.Join(context.Canceled, syscall.EIO)}, FailureLocalSystem, false},
	}
	long := error(context.Canceled)
	for range 20 {
		long = fmt.Errorf("wrapper: %w", long)
	}
	cases = append(cases, struct {
		name      string
		err       error
		class     FailureClass
		permanent bool
	}{"over_budget", long, FailureUnknown, false})
	wide := make([]error, 17)
	for index := range wide {
		wide[index] = context.Canceled
	}
	cases = append(cases, struct {
		name      string
		err       error
		class     FailureClass
		permanent bool
	}{"wide_over_budget", errors.Join(wide...), FailureUnknown, false})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got.Class != tc.class || got.Permanent != tc.permanent {
				t.Fatalf("class=%s permanent=%v", got.Class, got.Permanent)
			}
		})
	}
	failure := Failure{Class: FailureDNS, Cause: recoveryErrorSpy{}}
	if failure.Error() != "network carrier dns failure" {
		t.Fatal("finite failure prose changed")
	}
	if Classify(failure).Class != FailureDNS {
		t.Fatal("single-owner typed DNS class lost")
	}
}

func TestNetworkRecoveryAutomaticIncidentAndConfirmedRecovery(t *testing.T) {
	testAutomaticRecoveryReference(t, "support_01234567-89ab-4def-8123-456789abcdef")
}
func TestNetworkRecoveryAutomaticFallbackReference(t *testing.T) {
	testAutomaticRecoveryReference(t, "")
}
func testAutomaticRecoveryReference(t *testing.T, reference string) {
	t.Helper()
	clock := newFakeClock()
	replacer := &scriptedReplacer{results: []error{syscall.EIO, syscall.EIO}, ready: map[uint64]ReplacementResult{1: readyResult(identity(), 1, 11)}}
	controller := newControllerForTest(t, clock, replacer)
	recorder := diagnostics.NewMemoryRecorder()
	ctx := diagnostics.WithRecorder(supportref.WithContext(context.Background(), reference), recorder)
	var mu sync.Mutex
	faults := []errorreport.Fault{}
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { mu.Lock(); faults = append(faults, f); mu.Unlock() })
	defer restore()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(faults) }
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop(context.Background())
	observe(t, controller, 1, ReasonDNS, true)
	waitForDebounceTimer(t, controller)
	clock.Advance(100 * time.Millisecond)
	waitFor(t, func() bool { return controller.Health().Attempt == 1 && count() == 1 })
	mu.Lock()
	fault := faults[0]
	mu.Unlock()
	if reference == "" {
		reference = fault.SupportReference
	}
	if fault.Errno != int(syscall.EIO) || !supportref.Valid(reference) || fault.SupportReference != reference {
		t.Fatal("automatic owner lost original cause/reference")
	}
	// Retry timers can be armed after the health update; wait for their exact deadline.
	waitRetry := func() {
		waitFor(t, func() bool {
			controller.mu.RLock()
			deadline := controller.retryAt
			timer := controller.retryTimer
			controller.mu.RUnlock()
			fake := timer.(*fakeTimer)
			clock.mu.Lock()
			defer clock.mu.Unlock()
			return fake.active && fake.deadline.Equal(deadline)
		})
	}
	waitRetry()
	clock.Advance(100 * time.Millisecond)
	waitFor(t, func() bool { return controller.Health().Attempt == 2 })
	if count() != 1 {
		t.Fatal("transient retry emitted duplicate incident")
	}
	waitRetry()
	clock.Advance(200 * time.Millisecond)
	waitFor(t, func() bool { return controller.Health().State == StateReady && len(recorder.Recent()) == 1 })
	event := recorder.Recent()[0]
	if event.Code != "carrier_recovered" || event.SupportReference != reference {
		t.Fatal("confirmed recovery lost event/reference")
	}
	// Manual caller-owned replacement errors never gain another controller owner.
	replacer.mu.Lock()
	replacer.results = []error{syscall.EIO}
	replacer.mu.Unlock()
	observe(t, controller, 2, ReasonDNS, true)
	err := controller.Flush(ctx)
	if !errors.Is(err, syscall.EIO) || count() != 1 {
		t.Fatal("manual failure was swallowed or double owned")
	}
}

type recoveryStartFailureSource struct {
	fakeSource
	primary error
}

func (s *recoveryStartFailureSource) Start(context.Context) (<-chan Observation, error) {
	return nil, s.primary
}

func TestNetworkRecoverySourceFailureCleanupAndBoundedJoin(t *testing.T) {
	primary := recoveryErrorSpy{}
	source := &recoveryStartFailureSource{fakeSource: fakeSource{closed: make(chan struct{}), closeErr: syscall.EIO}, primary: primary}
	controller, err := New(Config{Identity: identity(), Replacer: &scriptedReplacer{}, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	err = controller.Start(t.Context())
	if !errors.Is(err, primary) || !errors.Is(err, syscall.EIO) || err.Error() != "network carrier recovery failed" {
		t.Fatal("source startup lost private primary/cleanup")
	}
	// A live source that disappears has one automatic final owner; Stop joins
	// both the controller and source consumer before returning.
	live := &fakeSource{events: make(chan Observation, 2), closed: make(chan struct{})}
	controller, err = New(Config{Identity: identity(), Replacer: &scriptedReplacer{}, Source: live})
	if err != nil {
		t.Fatal(err)
	}
	faults := make(chan errorreport.Fault, 3)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults <- f })
	defer restore()
	if err := controller.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	live.events <- Observation{} // Invalid monitor output is an owned permanent source fault.
	select {
	case <-faults:
	case <-time.After(time.Second):
		t.Fatal("invalid source output lost")
	}
	close(live.events)
	waitFor(t, func() bool { controller.mu.RLock(); defer controller.mu.RUnlock(); return controller.sourceFailed })
	select {
	case <-faults:
		t.Fatal("same source incident captured repeatedly")
	default:
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := controller.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.sourceDone:
	default:
		t.Fatal("source consumer remained after stop")
	}
	select {
	case <-controller.runDone:
	default:
		t.Fatal("replacement loop remained after stop")
	}
}

type recoveryShutdownReplacer struct{ started chan struct{} }

func (r recoveryShutdownReplacer) Replace(ctx context.Context, _ ReplacementRequest) (ReplacementResult, error) {
	close(r.started)
	<-ctx.Done()
	return ReplacementResult{}, errors.Join(ctx.Err(), syscall.EIO)
}

func TestNetworkRecoveryShutdownRetainsIndependentInFlightFailure(t *testing.T) {
	clock := newFakeClock()
	started := make(chan struct{})
	controller := newControllerForTest(t, clock, recoveryShutdownReplacer{started})
	if err := controller.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	observe(t, controller, 1, ReasonDefaultRoute, true)
	waitForDebounceTimer(t, controller)
	clock.Advance(100 * time.Millisecond)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("replacement did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := controller.Stop(ctx)
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown discarded independent replacement cause")
	}
	if controller.Health().State != StateStopped {
		t.Fatal("late failure regressed stopped state")
	}
	select {
	case <-controller.runDone:
	default:
		t.Fatal("shutdown did not join replacement loop")
	}
}
