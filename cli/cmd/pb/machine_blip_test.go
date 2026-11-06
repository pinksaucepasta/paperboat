package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestRetryWhileMachineOfflineWaitsOutABlipAndNothingElse(t *testing.T) {
	grace, interval := machineBlipGrace, machineBlipInterval
	machineBlipGrace, machineBlipInterval = 2*time.Second, 5*time.Millisecond
	t.Cleanup(func() { machineBlipGrace, machineBlipInterval = grace, interval })
	offline := &api.APIError{Code: "machine_offline"}

	calls := 0
	value, err := retryWhileMachineOffline(context.Background(), func() (int, error) {
		calls++
		if calls < 4 {
			return 0, offline
		}
		return 7, nil
	})
	if err != nil || value != 7 || calls != 4 {
		t.Fatalf("a blip was not waited out: value=%d err=%v calls=%d", value, err, calls)
	}

	other := errors.New("destination machine is revoked")
	calls = 0
	if _, err := retryWhileMachineOffline(context.Background(), func() (int, error) { calls++; return 0, other }); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("another error was retried: err=%v calls=%d", err, calls)
	}

	machineBlipGrace = 60 * time.Millisecond
	calls = 0
	if _, err := retryWhileMachineOffline(context.Background(), func() (int, error) { calls++; return 0, offline }); !machineOfflineError(err) || calls < 2 || calls > 20 {
		t.Fatalf("a machine that stays offline did not report it when the grace ended: err=%v calls=%d", err, calls)
	}

	machineBlipGrace = 5 * time.Second
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := retryWhileMachineOffline(cancelled, func() (int, error) { return 0, offline }); !machineOfflineError(err) || time.Since(started) > time.Second {
		t.Fatalf("a cancelled command kept waiting: %v", err)
	}
}
