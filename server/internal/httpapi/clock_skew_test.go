package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
)

// A machine whose clock is off gets a clock_skew answer with the server's time, not the 401 that makes
// a client treat the machine as signed out. Other proof denials stay 401, and every strict classifier
// still sees clock skew as a proof denial (never an outage).
func TestClockSkewIsAnsweredApartFromProofDenials(t *testing.T) {
	answer := func(err error) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		writeMachineProofFailure(w, httptest.NewRequest(http.MethodPost, "/v1/machine-events/wait", nil), err, "machine_identity_invalid", "rejected")
		return w
	}
	skew := answer(controlplane.ErrClockSkew)
	if skew.Code != http.StatusBadRequest || !strings.Contains(skew.Body.String(), `"code":"clock_skew"`) || !strings.Contains(skew.Body.String(), `"server_time"`) {
		t.Fatalf("clock skew = %d %s", skew.Code, skew.Body.String())
	}
	if denied := answer(controlplane.ErrHelperProof); denied.Code != http.StatusUnauthorized {
		t.Fatalf("plain proof denial = %d", denied.Code)
	}
	if !machineProofRejected(controlplane.ErrClockSkew) {
		t.Fatal("clock skew is not seen as a proof denial by the strict classifier")
	}
	w := httptest.NewRecorder()
	writePreviewMachineProofFailure(w, httptest.NewRequest(http.MethodPost, "/v1/previews", nil), controlplane.ErrClockSkew)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "clock_skew") {
		t.Fatalf("preview path clock skew = %d %s", w.Code, w.Body.String())
	}
}

// Identity renewal answers a skewed proof as clock_skew with the server's time, and keeps 401 for a machine or credential
// that is gone: the client stops renewing on 401, so a clock that is merely off must not get it.
func TestHelperRenewalAnswersClockSkewApartFromARemovedMachine(t *testing.T) {
	answer := func(err error) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		writeHelperRenewalFailure(w, httptest.NewRequest(http.MethodPost, "/v1/helper-identity-renewals", nil), err)
		return w
	}
	if skew := answer(controlplane.ErrClockSkew); skew.Code != http.StatusBadRequest || !strings.Contains(skew.Body.String(), `"code":"clock_skew"`) || !strings.Contains(skew.Body.String(), `"server_time"`) {
		t.Fatalf("skewed renewal = %d %s", skew.Code, skew.Body.String())
	}
	if removed := answer(controlplane.ErrHelperProof); removed.Code != http.StatusUnauthorized || !strings.Contains(removed.Body.String(), "credential_invalid") {
		t.Fatalf("refused renewal = %d %s", removed.Code, removed.Body.String())
	}
	if down := answer(errors.New("database unavailable")); down.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage = %d", down.Code)
	}
}
