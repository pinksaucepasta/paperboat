package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSchedulerChecksImmediatelyAndUsesBoundedJitter(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var randomErr error
	scheduler, err := New(Config{
		Check: func(context.Context) (Result, error) { return Result{Version: "2026.08.18.1", Updated: true}, nil },
		Now:   func() time.Time { return now }, Random: func(time.Duration) (time.Duration, error) { return time.Hour, randomErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := scheduler.CheckNow(context.Background())
	state := scheduler.Snapshot()
	if err != nil || !result.Updated || result.Version != "2026.08.18.1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if state.NextCheckAt != now.Add(4*time.Hour) || state.Failures != 0 || state.Failure != "" {
		t.Fatalf("state=%+v", state)
	}
	// Random-source outage retains the successful check and the established
	// unjittered fallback, while its independent consumed cause is visible.
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	randomErr = syscall.EIO
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal("jitter fallback failed successful update check")
	}
	if state = scheduler.Snapshot(); state.NextCheckAt != now.Add(DefaultInterval) || len(faults) != 1 || faults[0].Errno != int(syscall.EIO) {
		t.Fatal("random-source fallback cause/policy lost")
	}

}

func TestSchedulerWakeAppliesPreferencesWithoutRestart(t *testing.T) {
	calls := make(chan struct{}, 4)
	scheduler, err := New(Config{Check: func(context.Context) (Result, error) {
		calls <- struct{}{}
		return Result{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("scheduler did not stop")
		}
	}()
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("initial check missing")
	}
	scheduler.Wake()
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("settings change did not wake scheduler")
	}
}

func TestSchedulerCalendarWindowOnlyBringsCheckForward(t *testing.T) {
	now := time.Date(2026, 10, 8, 3, 59, 0, 0, time.UTC)
	wanted := now.Add(time.Minute)
	scheduler, err := New(Config{
		Check:     func(context.Context) (Result, error) { return Result{}, nil },
		Now:       func() time.Time { return now },
		Random:    func(time.Duration) (time.Duration, error) { return 0, nil },
		NextCheck: func(time.Time, time.Time) time.Time { return wanted },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := scheduler.Snapshot().NextCheckAt; !got.Equal(wanted) {
		t.Fatalf("calendar check=%v want=%v", got, wanted)
	}
	wanted = now.Add(24 * time.Hour)
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := scheduler.Snapshot().NextCheckAt; !got.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("calendar postponed availability check: %v", got)
	}
}

func TestSchedulerBackoffIsBoundedAndSuccessResetsFailures(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var attempts atomic.Uint32
	scheduler, err := New(Config{
		Check: func(context.Context) (Result, error) {
			if attempts.Add(1) < 4 {
				return Result{}, errors.New("offline")
			}
			return Result{Version: "2026.08.18.1"}, nil
		},
		Now: func() time.Time { return now }, Random: func(time.Duration) (time.Duration, error) { return 0, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	wants := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute}
	for _, want := range wants {
		if _, err := scheduler.CheckNow(context.Background()); err == nil {
			t.Fatal("failure was not returned")
		}
		if got := scheduler.Snapshot().NextCheckAt.Sub(now); got != want {
			t.Fatalf("delay=%v want=%v", got, want)
		}
	}
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := scheduler.Snapshot()
	if state.Failures != 0 || state.Failure != "" || state.NextCheckAt.Sub(now) != 3*time.Hour {
		t.Fatalf("state=%+v", state)
	}
}

func TestObserveCheckRecordsReadOnlySuccessWithoutCallingConfiguredCheck(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	var automaticCalls atomic.Uint32
	scheduler, err := New(Config{
		Check: func(context.Context) (Result, error) {
			automaticCalls.Add(1)
			return Result{Version: "activated", Updated: true}, nil
		},
		Now:    func() time.Time { return now },
		Random: func(time.Duration) (time.Duration, error) { return 0, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.ObserveCheck(context.Background(), func(context.Context) (Result, error) {
		return Result{}, errors.New("offline")
	}); err == nil {
		t.Fatal("read-only failure was not returned")
	}
	result, err := scheduler.ObserveCheck(context.Background(), func(context.Context) (Result, error) {
		return Result{Version: "2026.09.02.6"}, nil
	})
	if err != nil || result.Version != "2026.09.02.6" || result.Updated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if automaticCalls.Load() != 0 {
		t.Fatalf("configured activation check called %d times", automaticCalls.Load())
	}
	state := scheduler.Snapshot()
	if state.Version != result.Version || state.Updated || state.Failure != "" || state.Failures != 0 {
		t.Fatalf("state=%+v", state)
	}
}

func TestSchedulerCancellationStopsPromptly(t *testing.T) {
	started := make(chan struct{})
	scheduler, err := New(Config{Check: func(ctx context.Context) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, errors.Join(ctx.Err(), syscall.EIO)
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) {
			t.Fatal("canceled check lost independent I/O cause")
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestSchedulerRejectsUnsafeBounds(t *testing.T) {
	for _, config := range []Config{
		{},
		{Check: func(context.Context) (Result, error) { return Result{}, nil }, Interval: time.Second},
		{Check: func(context.Context) (Result, error) { return Result{}, nil }, RetryFloor: time.Hour, RetryLimit: time.Minute},
	} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config=%+v err=%v", config, err)
		}
	}
}

func TestSchedulerReportsBusyAndRetriesWithoutFailureBackoff(t *testing.T) {
	now := time.Now().UTC()
	busy := true
	scheduler, err := New(Config{Now: func() time.Time { return now }, Check: func(context.Context) (Result, error) {
		if busy {
			return Result{}, &ActiveTerminalSessionsError{RequiredVersion: "2026.09.07.1"}
		}
		return Result{Version: "2026.09.07.1", Updated: true}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = scheduler.CheckNow(context.Background()); err == nil {
			t.Fatal("busy omitted")
		}
		state := scheduler.Snapshot()
		if state.BlockedReason != BlockedActiveTerminalSessions || state.RequiredVersion != "2026.09.07.1" || state.Failures != 0 || state.Failure != "" || state.Updated || !state.NextCheckAt.Equal(now.Add(DefaultRetryFloor)) {
			t.Fatalf("state=%+v", state)
		}
	}
	busy = false
	if _, err = scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := scheduler.Snapshot()
	if state.BlockedReason != "" || state.RequiredVersion != "" || !state.Updated {
		t.Fatalf("success=%+v", state)
	}
}

func TestSchedulerRunHonorsRestoredBusyDeadline(t *testing.T) {
	var calls atomic.Uint32
	scheduler, err := New(Config{Check: func(context.Context) (Result, error) { calls.Add(1); return Result{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = scheduler.SeedBlockedActiveTerminalSessions("2026.09.07.1", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = scheduler.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("restart ignored restored retry deadline")
	}
}

func TestSchedulerRetainsDeferredIntentWhenCheckMakesNoProgress(t *testing.T) {
	now := time.Now().UTC()
	scheduler, err := New(Config{Now: func() time.Time { return now }, Check: func(context.Context) (Result, error) { return Result{Version: "14"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = scheduler.SeedBlockedActiveTerminalSessions("15", now); err != nil {
		t.Fatal(err)
	}
	if _, err = scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := scheduler.Snapshot()
	if state.RequiredVersion != "15" || state.BlockedReason != BlockedActiveTerminalSessions || !state.NextCheckAt.Equal(now.Add(DefaultRetryFloor)) {
		t.Fatalf("deferred intent lost: %+v", state)
	}
}

func TestSchedulerClearsDeferredIntentOnlyAfterSuccessfulActivation(t *testing.T) {
	for _, tc := range []struct {
		name, version        string
		updated, wantPending bool
	}{
		{"required merely found", "15", false, true},
		{"newer activated", "16", true, false},
		{"required activated", "15", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheduler, err := New(Config{Check: func(context.Context) (Result, error) { return Result{Version: tc.version, Updated: tc.updated}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			if err = scheduler.SeedBlockedActiveTerminalSessions("15", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err = scheduler.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			if pending := scheduler.Snapshot().RequiredVersion != ""; pending != tc.wantPending {
				t.Fatalf("pending=%t", pending)
			}
		})
	}
}

func TestSchedulerMixedMaintenanceFailureUsesBackoffAndRecovers(t *testing.T) {
	now := time.Now().UTC()
	failure := errors.Join(&OwnerMaintenancePendingError{Notice: OwnerMaintenanceNotice{Version: "2026.10.08.1", Deadline: now.Add(time.Hour)}}, syscall.EIO)
	scheduler, err := New(Config{Now: func() time.Time { return now }, Random: func(time.Duration) (time.Duration, error) { return 0, nil }, Check: func(context.Context) (Result, error) { return Result{}, failure }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scheduler.CheckNow(context.Background()); !errors.Is(err, syscall.EIO) {
		t.Fatal("original cause missing")
	}
	state := scheduler.Snapshot()
	if state.Failures != 1 || state.Failure != "check_failed" || state.BlockedReason != "" {
		t.Fatal("independent failure was hidden as expected maintenance")
	}
	failure = nil
	if _, err = scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state = scheduler.Snapshot(); state.Failures != 0 || state.Failure != "" {
		t.Fatal("healthy check failed to reset episode")
	}
	// Ordinary wrapped ownership remains an expected blocker.
	failure = fmt.Errorf("wrapper: %w", &ActiveTerminalSessionsError{RequiredVersion: "2026.10.08.1"})
	if _, err = scheduler.CheckNow(context.Background()); err == nil {
		t.Fatal("blocker not returned")
	}
	if state = scheduler.Snapshot(); state.BlockedReason != BlockedActiveTerminalSessions || state.Failures != 0 {
		t.Fatal("pure blocker policy changed")
	}
}

func TestSchedulerAutomaticFailureEpisodesRemainCorrelatedAndManualChecksStayOwned(t *testing.T) {
	faults := make(chan errorreport.Fault, 4)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults <- f })
	defer restore()
	results := make(chan error, 4)
	observed := make(chan struct{}, 4)
	scheduler, err := New(Config{Check: func(context.Context) (Result, error) { return Result{}, <-results }, Observe: func(Observation) { observed <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	// The manual owner receives the error and gets no duplicate local publication.
	results <- syscall.EIO
	if _, err = scheduler.CheckNow(context.Background()); !errors.Is(err, syscall.EIO) {
		t.Fatal("manual cause lost")
	}
	<-observed
	if len(faults) != 0 {
		t.Fatal("manual check was published twice")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	results <- syscall.EIO
	go func() { done <- scheduler.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("automatic owner failed to join")
		}
	}()
	nextObserved := func() {
		t.Helper()
		select {
		case <-observed:
		case <-time.After(time.Second):
			t.Fatal("check did not finish")
		}
	}
	nextFault := func() errorreport.Fault {
		t.Helper()
		select {
		case f := <-faults:
			return f
		case <-time.After(time.Second):
			t.Fatal("automatic failure was not observed")
			return errorreport.Fault{}
		}
	}
	// Wake bypasses the existing retry delay without changing production policy.
	scheduler.Wake()
	nextObserved()
	first := nextFault()
	if !supportref.Valid(first.SupportReference) || first.Errno != int(syscall.EIO) {
		t.Fatal("automatic native cause/reference lost")
	}
	results <- syscall.EIO
	scheduler.Wake()
	nextObserved()
	results <- nil
	scheduler.Wake()
	nextObserved()
	// An attempt already recorded by the production HTTP transport owns its
	// entire failure; the scheduler must not mirror it as a second local fault.
	results <- errorreport.RecordedHTTPFailure(syscall.EIO)
	scheduler.Wake()
	nextObserved()
	results <- nil
	scheduler.Wake()
	nextObserved()
	results <- syscall.EIO
	scheduler.Wake()
	nextObserved()
	second := nextFault()
	if second.SupportReference != first.SupportReference || len(faults) != 0 {
		t.Fatal("episode reset/correlation or duplicate observation failed")
	}
}
