package native

import (
	"context"
	"errors"
	"net/http"
	"testing"

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
