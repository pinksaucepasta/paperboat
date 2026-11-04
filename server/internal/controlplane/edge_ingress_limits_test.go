package controlplane

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
)

// F21-47: an edge service nobody configured still answers heartbeats with the shared default
// set (the zero value would fail the edge's validation), and the answer is the same for every edge.
func TestHeartbeatAnswerCarriesTheSharedDefaultLimits(t *testing.T) {
	service := NewEdgeService(nil, "credential")
	var first, second edgelimits.IngressLimits
	for _, out := range []*edgelimits.IngressLimits{&first, &second} {
		recorder := httptest.NewRecorder()
		writeHeartbeatAnswer(recorder, service.ingressLimits)
		var answer struct {
			IngressLimits edgelimits.IngressLimits `json:"ingress_limits"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		*out = answer.IngressLimits
	}
	if first != edgelimits.Default() || second != first || first.Validate() != nil {
		t.Fatalf("heartbeat limits = %+v", first)
	}
}
