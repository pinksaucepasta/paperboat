//go:build windows

package updated

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestWaitWindowsRuntimeStatusToleratesASlowFirstHeartbeat(t *testing.T) {
	journal := testWindowsActivationJournal()
	journal.Version = "2026.11.10.6"
	journal.Release = workerupdate.Release{HostdAPIMin: 1, HostdAPIMax: 2}
	healthy := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-2026.11.10.6", Epoch: 1, APIVersion: 2, LastHeartbeatUnixMilli: time.Now().UnixMilli()}
	calls := 0
	read := func(context.Context) (hostdproto.Status, error) {
		calls++
		if calls < 4 {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: healthy.WorkerID}, nil // no heartbeat yet
		}
		healthy.LastHeartbeatUnixMilli = time.Now().UnixMilli()
		return healthy, nil
	}
	if err := waitWindowsRuntimeStatus(context.Background(), read, journal, time.Second, time.Millisecond); err != nil || calls != 4 {
		t.Fatalf("slow first heartbeat: err=%v calls=%d", err, calls)
	}
}

func TestWaitWindowsRuntimeStatusNamesWhatItLastSaw(t *testing.T) {
	journal := testWindowsActivationJournal()
	journal.Version = "2026.11.10.6"
	journal.Release = workerupdate.Release{HostdAPIMin: 1, HostdAPIMax: 2}
	read := func(context.Context) (hostdproto.Status, error) {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "runtime-other", Epoch: 3, APIVersion: 2, LastHeartbeatUnixMilli: time.Now().UnixMilli()}, nil
	}
	err := waitWindowsRuntimeStatus(context.Background(), read, journal, 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, errInvalidWindowsActivation) || !strings.Contains(err.Error(), "worker=runtime-other") || !strings.Contains(err.Error(), "epoch=3") {
		t.Fatalf("error does not name the observed status: %v", err)
	}
}
