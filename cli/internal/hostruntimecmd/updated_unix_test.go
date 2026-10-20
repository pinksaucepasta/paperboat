//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestValidRuntimeIdentitySupportsOnlyExactPairs(t *testing.T) {
	for _, test := range []struct {
		uid, gid int
		want     bool
	}{
		{uid: 1000, gid: 1000, want: true},
		{uid: 0, gid: 0, want: true},
		{uid: 0, gid: 1000},
		{uid: 1000, gid: 0},
		{uid: -1, gid: -1},
	} {
		if got := validRuntimeIdentity(test.uid, test.gid); got != test.want {
			t.Fatalf("validRuntimeIdentity(%d, %d)=%v want %v", test.uid, test.gid, got, test.want)
		}
	}
}

type recordingUpdaterRunner struct {
	started chan struct{}
	stopped chan struct{}
}

func (r *recordingUpdaterRunner) Run(ctx context.Context, ready func() error) error {
	close(r.started)
	if err := ready(); err != nil {
		return err
	}
	<-ctx.Done()
	close(r.stopped)
	return ctx.Err()
}

type recordingProcessNotifier struct {
	mu               sync.Mutex
	events           []string
	watchdogInterval time.Duration
	watchdogSeen     chan struct{}
	watchdogErr      error
}

func (n *recordingProcessNotifier) Ready() error {
	n.record("ready")
	return nil
}

func (n *recordingProcessNotifier) Degraded(_ string) error {
	n.record("degraded")
	return nil
}

func (n *recordingProcessNotifier) Draining() error {
	n.record("draining")
	return nil
}
func (n *recordingProcessNotifier) Stopping() error {
	n.record("stopping")
	return nil
}

func (n *recordingProcessNotifier) WatchdogInterval() time.Duration { return n.watchdogInterval }

func (n *recordingProcessNotifier) Watchdog() error {
	n.record("watchdog")
	select {
	case n.watchdogSeen <- struct{}{}:
	default:
	}
	return n.watchdogErr
}

func (n *recordingProcessNotifier) record(event string) {
	n.mu.Lock()
	n.events = append(n.events, event)
	n.mu.Unlock()
}

func (n *recordingProcessNotifier) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.events...)
}

func TestRunNotifiedUpdaterSendsReadyWatchdogAndStopping(t *testing.T) {
	runner := &recordingUpdaterRunner{started: make(chan struct{}), stopped: make(chan struct{})}
	notifier := &recordingProcessNotifier{watchdogInterval: time.Millisecond, watchdogSeen: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runNotifiedUpdater(ctx, runner, notifier) }()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("updater did not start")
	}
	select {
	case <-notifier.watchdogSeen:
	case <-time.After(time.Second):
		t.Fatal("watchdog notification was not sent")
	}
	cancel()
	select {
	case <-runner.stopped:
	case <-time.After(time.Second):
		t.Fatal("updater did not stop after cancellation")
	}
	if err := <-done; err != nil {
		t.Fatalf("a requested stop must be a clean exit, got %v", err)
	}
	events := notifier.snapshot()
	if len(events) < 3 || events[0] != "ready" || events[len(events)-1] != "stopping" {
		t.Fatalf("notification events=%v", events)
	}
}

func TestRunNotifiedUpdaterStopsAfterWatchdogFailure(t *testing.T) {
	watchdogErr := errors.New("watchdog socket failed")
	runner := &recordingUpdaterRunner{started: make(chan struct{}), stopped: make(chan struct{})}
	notifier := &recordingProcessNotifier{watchdogInterval: time.Millisecond, watchdogSeen: make(chan struct{}, 1), watchdogErr: watchdogErr}
	if err := runNotifiedUpdater(context.Background(), runner, notifier); !errors.Is(err, watchdogErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
	select {
	case <-runner.stopped:
	case <-time.After(time.Second):
		t.Fatal("updater did not stop after watchdog failure")
	}
	events := notifier.snapshot()
	if len(events) < 4 || events[0] != "ready" || events[len(events)-1] != "stopping" {
		t.Fatalf("notification events=%v", events)
	}
	if events[len(events)-3] != "degraded" || events[len(events)-2] != "draining" {
		t.Fatalf("notification events=%v", events)
	}
}

func TestRunNotifiedUpdaterRejectsExitBeforeReadiness(t *testing.T) {
	readyNeverCalled := func(context.Context, func() error) error { return nil }
	notifier := &recordingProcessNotifier{watchdogSeen: make(chan struct{}, 1)}
	err := runNotifiedUpdater(context.Background(), updaterRunnerFunc(readyNeverCalled), notifier)
	if !errors.Is(err, errUpdaterExitedBeforeReadiness) {
		t.Fatalf("run error=%v", err)
	}
	if got := notifier.snapshot(); len(got) != 2 || got[0] != "degraded" || got[1] != "stopping" {
		t.Fatalf("notification events=%v", got)
	}
}

func TestUpdaterStopRequestedByTheServiceManagerIsNotAFailure(t *testing.T) {
	runner := &recordingUpdaterRunner{started: make(chan struct{}), stopped: make(chan struct{})}
	notifier := &recordingProcessNotifier{watchdogSeen: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runNotifiedUpdater(ctx, runner, notifier) }()
	<-runner.started
	cancel() // systemd stop: SIGTERM cancels the service context
	if err := <-done; err != nil {
		t.Fatalf("a requested stop exited with failure: %v", err)
	}
	// A real failure during the stop still fails.
	failing := errors.New("drain notification failed")
	if err := updaterShutdownResult(ctx, context.Canceled, failing); err == nil {
		t.Fatal("a failed shutdown step was hidden")
	}
}

// The stop request cancels the runner as well, so the runner's return can arrive together with the
// context's cancellation; whichever the loop sees first, a requested stop is not a failure.
func TestUpdaterStopIsCleanWhicheverCancellationArrivesFirst(t *testing.T) {
	for range 300 {
		ctx, cancel := context.WithCancel(context.Background())
		runner := updaterRunnerFunc(func(runCtx context.Context, ready func() error) error {
			if err := ready(); err != nil {
				return err
			}
			cancel() // the service manager's stop and the runner's own return become visible together
			<-runCtx.Done()
			return runCtx.Err()
		})
		notifier := &recordingProcessNotifier{watchdogSeen: make(chan struct{}, 1)}
		if err := runNotifiedUpdater(ctx, runner, notifier); err != nil {
			t.Fatalf("a requested stop exited with failure: %v", err)
		}
	}
}
