package machineguard

import (
	"errors"
	"testing"
)

// Every gateway connection not handed to a daemon is counted by its reason (pbmac F23-02: one empty answer
// under churn could not be told apart from a timeout).
func TestGatewayRefusalsAreCountedByReason(t *testing.T) {
	before := gatewayRefusalSnapshot()
	for _, err := range []error{errAttributionAmbiguous, errAttributionAmbiguous, errAttributionNoDaemon, errors.New("pcblist read failed")} {
		countGatewayRefusal(attributionRefusalReason(err), 443, err)
	}
	countGatewayRefusal(refusalBusy, 80, nil)
	after := gatewayRefusalSnapshot()
	for reason, want := range map[string]uint64{refusalAmbiguous: 2, refusalNoDaemon: 1, refusalLookupFailed: 1, refusalBusy: 1} {
		if got := after[reason] - before[reason]; got != want {
			t.Fatalf("%s counted %d, want %d (%v)", reason, got, want, after)
		}
	}
}
