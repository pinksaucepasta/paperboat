package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
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
