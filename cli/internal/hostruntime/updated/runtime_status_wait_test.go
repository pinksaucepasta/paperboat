package updated

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

const waitVersion = "2026.11.10.6"

func healthyStatus() hostdproto.Status {
	return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-" + waitVersion, Epoch: 1, APIVersion: 2, LastHeartbeatUnixMilli: time.Now().UnixMilli()}
}

func TestWaitWindowsRuntimeStatusToleratesASlowFirstHeartbeat(t *testing.T) {
	calls := 0
	read := func(context.Context) (hostdproto.Status, error) {
		calls++
		if calls < 4 {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-" + waitVersion}, nil // no heartbeat yet
		}
		return healthyStatus(), nil
	}
	if err := waitWindowsRuntimeStatus(context.Background(), read, waitVersion, 1, 2, time.Second, time.Millisecond); err != nil || calls != 4 {
		t.Fatalf("slow first heartbeat: err=%v calls=%d", err, calls)
	}
}

func TestWaitWindowsRuntimeStatusToleratesATransientReadError(t *testing.T) {
	calls := 0
	read := func(context.Context) (hostdproto.Status, error) {
		calls++
		if calls == 1 {
			return hostdproto.Status{}, errors.New("pipe busy")
		}
		return healthyStatus(), nil
	}
	if err := waitWindowsRuntimeStatus(context.Background(), read, waitVersion, 1, 2, time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestWaitWindowsRuntimeStatusRefusesWhatIsNotTheNewRuntime(t *testing.T) {
	stale := healthyStatus()
	stale.WorkerID = "runtime-2026.11.10.5" // the previous version, still fresh
	outOfRange := healthyStatus()
	outOfRange.APIVersion = 3
	for name, status := range map[string]hostdproto.Status{"previous runtime": stale, "api out of range": outOfRange} {
		read := func(context.Context) (hostdproto.Status, error) { return status, nil }
		err := waitWindowsRuntimeStatus(context.Background(), read, waitVersion, 1, 2, 20*time.Millisecond, time.Millisecond)
		if !errors.Is(err, errInvalidWindowsActivation) || !strings.Contains(err.Error(), "worker="+status.WorkerID) {
			t.Fatalf("%s accepted or unnamed: %v", name, err)
		}
	}
}

func TestWaitWindowsRuntimeStatusNamesWhatItLastSawAndWhyItStopped(t *testing.T) {
	readErr := errors.New("token file unreadable")
	read := func(context.Context) (hostdproto.Status, error) { return hostdproto.Status{}, readErr }
	err := waitWindowsRuntimeStatus(context.Background(), read, waitVersion, 1, 2, 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, errInvalidWindowsActivation) || !errors.Is(err, readErr) {
		t.Fatalf("read error hidden: %v", err)
	}
	noBeat := func(context.Context) (hostdproto.Status, error) {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-" + waitVersion, Epoch: 3, APIVersion: 2}, nil
	}
	err = waitWindowsRuntimeStatus(context.Background(), noBeat, waitVersion, 1, 2, 20*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "epoch=3") || !strings.Contains(err.Error(), "heartbeat_age=none") {
		t.Fatalf("missing heartbeat not described: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	counting := func(context.Context) (hostdproto.Status, error) { calls++; return hostdproto.Status{}, nil }
	err = waitWindowsRuntimeStatus(ctx, counting, waitVersion, 1, 2, time.Minute, time.Millisecond)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("canceled wait: err=%v calls=%d, want one read and the context error", err, calls)
	}
}
