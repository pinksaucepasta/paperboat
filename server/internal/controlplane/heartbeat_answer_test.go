package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
)

// T11: a recorded heartbeat answers 200 with exactly the configured ingress limits, nothing else, so an edge
// decoding strictly accepts it.
func TestHeartbeatAnswerIsExactlyTheIngressLimits(t *testing.T) {
	limits := edgelimits.Default()
	limits.Publication.Connections = 77
	recorder := httptest.NewRecorder()
	writeHeartbeatAnswer(recorder, limits)
	var answer struct {
		IngressLimits edgelimits.IngressLimits `json:"ingress_limits"`
	}
	decoder := json.NewDecoder(recorder.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answer); err != nil || recorder.Code != http.StatusOK || answer.IngressLimits != limits {
		t.Fatalf("answer status=%d err=%v limits=%+v, want 200 with %+v", recorder.Code, err, answer.IngressLimits, limits)
	}
}
