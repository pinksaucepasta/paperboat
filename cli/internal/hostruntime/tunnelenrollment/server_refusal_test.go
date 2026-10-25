package tunnelenrollment

import (
	"errors"
	"net/http/httptest"
	"testing"
)

// Staging, 2026-10-26: carrier-bootstrap answered 503 carrier_unavailable and `pb tunnel connector add` showed
// only local_io. The local endpoint now passes the server's code on and the local client restores it.
func TestCarrierBootstrapRefusalCrossesTheLocalEndpoint(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeServerRefusal(recorder, "carrier_unavailable")
	if recorder.Code != 503 || !safeServerCode("carrier_unavailable") || safeServerCode("Carrier unavailable: see https://x") {
		t.Fatalf("status=%d", recorder.Code)
	}
	var refused *CarrierBootstrapError
	if err := decodeLocalEnrollmentRefusal(recorder.Code, recorder.Body.Bytes()); !errors.As(err, &refused) || refused.Code != "carrier_unavailable" {
		t.Fatalf("err=%v", err)
	}
}
