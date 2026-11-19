package main

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// withMachineBlip is ctx with this machine-blip grace and interval; a zero grace keeps the production one.
func withMachineBlip(ctx context.Context, grace, interval time.Duration) context.Context {
	return withTimings(ctx, func(timings *commandTimings) {
		if grace > 0 {
			timings.machineBlipGrace = grace
		}
		timings.machineBlipInterval = interval
	})
}

// withExecReattach is ctx with this exec reattach attempt timeout and window; a zero value keeps the production one.
func withExecReattach(ctx context.Context, attempt, window time.Duration) context.Context {
	return withTimings(ctx, func(timings *commandTimings) {
		if attempt > 0 {
			timings.execReattachAttempt = attempt
		}
		if window > 0 {
			timings.execReattachWindow = window
		}
	})
}

// Runs that need different grace periods run at once, each with its own. With package variables the writes of the
// short run shortened the long one mid-flight (and the race detector failed the pair).
func TestMachineBlipTimingBelongsToEachRun(t *testing.T) {
	offline := &api.APIError{Code: "machine_offline"}
	t.Run("a short grace gives up while a long one still waits", func(t *testing.T) {
		t.Parallel()
		calls := 0
		ctx := withMachineBlip(t.Context(), 40*time.Millisecond, 2*time.Millisecond)
		if _, err := retryWhileMachineOffline(ctx, func() (int, error) { calls++; return 0, offline }); !machineOfflineError(err) || calls > 25 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
	t.Run("a long grace waits out a blip the short one would not", func(t *testing.T) {
		t.Parallel()
		calls := 0
		ctx := withMachineBlip(t.Context(), 10*time.Second, 2*time.Millisecond)
		value, err := retryWhileMachineOffline(ctx, func() (int, error) {
			if calls++; calls < 60 { // about 120 ms of offline answers, three times the short run's whole grace
				return 0, offline
			}
			return 7, nil
		})
		if err != nil || value != 7 || calls != 60 {
			t.Fatalf("value=%d err=%v calls=%d", value, err, calls)
		}
	})
}

func TestProductionTimingsApplyWithoutAContextValue(t *testing.T) {
	if got := timingsOf(t.Context()); got.machineBlipGrace != 15*time.Second || got.machineBlipInterval != 150*time.Millisecond ||
		got.execReattachWindow != 90*time.Second || got.tunnelRequestRetryDelay == nil {
		t.Fatalf("production timings %+v", got)
	}
}

// withTunnelDoctorProbe is ctx with this loopback health probe timeout.
func withTunnelDoctorProbe(ctx context.Context, probe time.Duration) context.Context {
	return withTimings(ctx, func(timings *commandTimings) { timings.tunnelDoctorProbe = probe })
}
