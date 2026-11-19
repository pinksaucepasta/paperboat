package main

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

func machineOfflineError(err error) bool {
	var remote *api.APIError
	return errors.As(err, &remote) && remote.Code == "machine_offline"
}

// retryWhileMachineOffline runs call again while it answers machine_offline, for at most the context's machine-blip grace.
func retryWhileMachineOffline[T any](ctx context.Context, call func() (T, error)) (T, error) {
	return retryWhile(ctx, timingsOf(ctx).machineBlipGrace, machineOfflineError, call)
}

// retryWhile runs call again while retryable says its error is a handover blip, until grace ends or ctx is done; the
// last result is returned either way.
func retryWhile[T any](ctx context.Context, grace time.Duration, retryable func(error) bool, call func() (T, error)) (T, error) {
	interval := timingsOf(ctx).machineBlipInterval
	deadline := time.Now().Add(grace)
	for {
		value, err := call()
		if err == nil || !retryable(err) || !time.Now().Add(interval).Before(deadline) {
			return value, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return value, err
		}
	}
}

// waitForMachineOnline reads the machine again while the server calls it offline, for at most the context's grace, and
// returns it once it is online; a machine that stays offline ends with the machine_offline error.
func waitForMachineOnline(ctx context.Context, resolve func() (api.UserMachine, error)) (api.UserMachine, error) {
	return retryWhileMachineOffline(ctx, func() (api.UserMachine, error) {
		machine, err := resolve()
		if err == nil && !machine.Online {
			err = &api.APIError{Code: "machine_offline"}
		}
		return machine, err
	})
}
