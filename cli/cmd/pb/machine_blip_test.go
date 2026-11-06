package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
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

func TestSSHHandoverBlipIsATransportLossNotARefusalOrATimeout(t *testing.T) {
	for name, want := range map[string]struct {
		err  error
		blip bool
	}{
		"transport lost":        {errors.Join(tunnel.ErrTransportLost, errors.New("closed")), true},
		"peer transport absent": {&localapi.RemoteError{Code: "peer_transport_unavailable"}, true},
		"deadline":              {context.DeadlineExceeded, false},
		"local service down":    {localapi.ErrTransportUnavailable, false},
		"other remote":          {&localapi.RemoteError{Code: "ssh_refused"}, false},
	} {
		if got := sshHandoverBlip(want.err); got != want.blip {
			t.Errorf("%s: blip=%v want %v", name, got, want.blip)
		}
	}
	interval := machineBlipInterval
	machineBlipInterval = 5 * time.Millisecond
	t.Cleanup(func() { machineBlipInterval = interval })
	calls := 0
	_, err := retryWhile(context.Background(), time.Second, sshHandoverBlip, func() (int, error) {
		calls++
		if calls < 3 {
			return 0, tunnel.ErrTransportLost
		}
		return 1, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("a transport blip was not retried: err=%v calls=%d", err, calls)
	}
}

// pb exec reattaches for about 30 s when its stream is cut; the server answering machine_offline to the new descriptor
// while the host restarts must not end that (pbvm4 update: exec_short exit 1 at the handover).
func TestExecReattachmentRetriesMachineOffline(t *testing.T) {
	if !retryableExecReattachment(&api.APIError{Code: "machine_offline"}) {
		t.Fatal("machine_offline ended the exec reattachment")
	}
	if retryableExecReattachment(&api.APIError{Code: "user_machine_not_found"}) {
		t.Fatal("a missing machine was retried")
	}
}
