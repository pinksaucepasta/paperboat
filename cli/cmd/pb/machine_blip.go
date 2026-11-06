package main

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// machineBlipGrace is how long a command that has not started its work waits for a machine the server calls offline.
// A host restarting its runtime (pb update) is reported unobserved or offline for a few seconds; pbvm4 update checks
// failed sends with machine_offline for the whole network handover. The wait only covers the offline answer: any
// other error returns at once, and a machine that stays offline reports it when the grace ends.
var machineBlipGrace = 15 * time.Second

var machineBlipInterval = 500 * time.Millisecond

func machineOfflineError(err error) bool {
	var remote *api.APIError
	return errors.As(err, &remote) && remote.Code == "machine_offline"
}

// retryWhileMachineOffline runs call again while it answers machine_offline, for at most machineBlipGrace.
func retryWhileMachineOffline[T any](ctx context.Context, call func() (T, error)) (T, error) {
	return retryWhile(ctx, machineBlipGrace, machineOfflineError, call)
}

// retryWhile runs call again while retryable says its error is a handover blip, until grace ends or ctx is done; the
// last result is returned either way.
func retryWhile[T any](ctx context.Context, grace time.Duration, retryable func(error) bool, call func() (T, error)) (T, error) {
	deadline := time.Now().Add(grace)
	for {
		value, err := call()
		if err == nil || !retryable(err) || !time.Now().Add(machineBlipInterval).Before(deadline) {
			return value, err
		}
		timer := time.NewTimer(machineBlipInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return value, err
		}
	}
}
