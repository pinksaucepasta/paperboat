package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// A send cut at the plan limit by a refused authorization renewal names the limit and the Billing link,
// not "could not finish the command" (Lane O, relay-only containers).
func TestARenewalRefusedAtThePlanLimitNamesIt(t *testing.T) {
	refused := &api.APIError{Status: http.StatusConflict, Code: "bandwidth_quota_exhausted", Message: "This workspace's hosted relay and edge bandwidth is used up. Add bandwidth: https://console.example.test/billing"}
	renewal := fmt.Errorf("file transfer authorization renewal failed: %w", refused)
	err := planLimitRefusal(context.Background(), errors.Join(errors.New("write unix local-api.sock: broken pipe"), renewal))
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	result := classifyCLIJSONError(err)
	if !strings.Contains(message, "Add bandwidth: https://console.example.test/billing") || result.Code != "bandwidth_quota_exhausted" {
		t.Fatalf("message=%q code=%q", message, result.Code)
	}
	if other := errors.New("dial"); planLimitRefusal(context.Background(), other) != other {
		t.Fatal("an unrelated failure changed")
	}
}

// A send the relay cut at the limit fails like a lost connection; while the daemon's latest refresh
// says relays are withheld at the limit, the command names it (Lane O, pbvm3 relay-only send).
func TestAStreamCutByTheRelayAtThePlanLimitNamesIt(t *testing.T) {
	const limit = "This workspace's hosted relay and edge bandwidth is used up. Add bandwidth: https://console.example.test/billing"
	previous := daemonSnapshotForPlanLimit
	t.Cleanup(func() { daemonSnapshotForPlanLimit = previous })
	health := []localapi.HealthItem{{Code: localapi.RelayQuotaCode, Severity: "warning", Title: limit}}
	daemonSnapshotForPlanLimit = func(context.Context) (localapi.Snapshot, error) { return localapi.Snapshot{Health: health}, nil }
	var lost error = fmt.Errorf("send: %w", &net.OpError{Op: "read", Net: "unix", Err: syscall.ECONNRESET})
	if failure := classifyCommandFailure(lost); failure.kind != commandOperational {
		t.Fatalf("fixture is not a connectivity failure: %+v", failure)
	}
	err := planLimitRefusal(context.Background(), lost)
	if message := commandFailureMessage(err, classifyCommandFailure(err)); !strings.Contains(message, "Add bandwidth: https://console.example.test/billing") {
		t.Fatalf("message=%q", message)
	}
	// exec ("could not complete the request") and send ("canceled") at the cap on a relay-only path
	// (Lane O, server .46): an unexpected failure and a cancellation the user did not ask for.
	for _, other := range []error{errors.New("local service failed"), context.Canceled} {
		err := planLimitRefusal(context.Background(), other)
		if message := commandFailureMessage(err, classifyCommandFailure(err)); !strings.Contains(message, "Add bandwidth") {
			t.Fatalf("%v: message=%q", other, message)
		}
	}
	// A send cut by the relay ends canceled with its context canceled too (Lane O, F13-03S): only a
	// real interrupt keeps "Operation canceled".
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := planLimitRefusal(canceledCtx, context.Canceled); err == context.Canceled {
		t.Fatal("a relay cut that canceled the command was reported as a user cancel")
	}
	userInterrupted.Store(true)
	t.Cleanup(func() { userInterrupted.Store(false) })
	if planLimitRefusal(canceledCtx, context.Canceled) != context.Canceled {
		t.Fatal("a cancellation by the user named the plan limit")
	}
	health = nil
	if planLimitRefusal(context.Background(), lost) != lost {
		t.Fatal("a lost connection without a relay refusal changed")
	}
}
