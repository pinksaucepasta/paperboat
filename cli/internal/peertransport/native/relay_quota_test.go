package native

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type fixedDenier struct{ denial *api.APIError }

func (d fixedDenier) RelayDenial() *api.APIError { return d.denial }

func TestConnectionFailureWithRelayAllowanceUsedUpNamesThePlanLimit(t *testing.T) {
	denial := &api.APIError{Status: http.StatusConflict, Code: "bandwidth_quota_exhausted", Message: "This workspace's hosted relay bandwidth is used up. Add bandwidth: https://console.example.test/billing"}
	connect := classifyPeerOperationFailure(context.Background(), "peer_connect", "native_private_failed", errors.New("no path"))
	got := relayQuotaFailure(fixedDenier{denial}, connect)
	var quota *RelayQuotaError
	if !errors.As(got, &quota) || quota.LocalAPICode() != "bandwidth_quota_exhausted" || quota.LocalAPIMessage() != denial.Message || quota.Error() != denial.Message {
		t.Fatalf("got %#v", got)
	}
	if !errors.Is(got, connect) {
		t.Fatal("the transport failure it replaces is no longer reachable")
	}
	authority := classifyPeerOperationFailure(context.Background(), "peer_authority", "peer_authority_failed", errors.New("revoked"))
	for name, test := range map[string]struct {
		denier relayDenier
		err    error
	}{
		"allowance available": {fixedDenier{}, connect},
		"authority rejection": {fixedDenier{denial}, authority},
		"not a peer failure":  {fixedDenier{denial}, errors.New("other")},
		"no denier":           {nil, connect},
		"nothing failed":      {fixedDenier{denial}, nil},
	} {
		if out := relayQuotaFailure(test.denier, test.err); out != test.err {
			t.Fatalf("%s: changed the error to %#v", name, out)
		}
	}
}

// F13-03: with relays withheld a dial that runs out of time is the plan limit, and the dial stops early
// enough for the daemon to say so before the caller's own deadline.
func TestTimedOutDialWithRelaysWithheldIsThePlanLimit(t *testing.T) {
	denial := &api.APIError{Status: http.StatusConflict, Code: "bandwidth_quota_exhausted", Message: "Hosted relay bandwidth is used up."}
	timedOut := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	var quota *RelayQuotaError
	if !errors.As(relayQuotaFailure(fixedDenier{denial}, timedOut), &quota) {
		t.Fatal("a timed-out dial with relays withheld was not named as the plan limit")
	}
	if out := relayQuotaFailure(fixedDenier{}, timedOut); out != timedOut {
		t.Fatal("a timeout with relays available changed")
	}
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want, _ := parent.Deadline()
	shorter, stop := relayWithheldDeadline(parent, fixedDenier{denial})
	got, _ := shorter.Deadline()
	stop()
	if !got.Equal(want.Add(-relayAnswerMargin)) {
		t.Fatalf("dial deadline %v, want %v", got, want.Add(-relayAnswerMargin))
	}
	same, stop := relayWithheldDeadline(parent, fixedDenier{})
	got, _ = same.Deadline()
	stop()
	if !got.Equal(want) {
		t.Fatal("the dial deadline changed although relays are available")
	}
	// A dial without a caller deadline (pb send) is bounded while relays are withheld, so it ends with
	// the plan limit instead of waiting for a relay that never comes (Lane O, F13-03S).
	bounded, stop := relayWithheldDeadline(context.Background(), fixedDenier{denial})
	got, ok := bounded.Deadline()
	stop()
	if !ok || time.Until(got) > relayWithheldDialTimeout {
		t.Fatalf("an unbounded dial stayed unbounded with relays withheld: %v %v", got, ok)
	}
	open, stop := relayWithheldDeadline(context.Background(), fixedDenier{})
	_, ok = open.Deadline()
	stop()
	if ok {
		t.Fatal("an unbounded dial got a deadline although relays are available")
	}
}
