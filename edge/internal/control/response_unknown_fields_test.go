package control

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/strictjson"
)

// The 2026-11-04 outage: server 2026.11.04.3 added "version" to the ingress answer, and every edge on
// 2026.11.04.2 rejected the whole answer (strict decoding), lost its routes and closed TLS. A control
// response with a field this edge does not know must still decode, and a wrong type must still fail.
func TestControlResponseWithAnUnknownFieldStillDecodes(t *testing.T) {
	var answer struct {
		Complete  bool     `json:"complete"`
		Decisions []string `json:"decisions"`
	}
	if err := strictjson.DecodeResponse([]byte(`{"complete":true,"decisions":["a"],"version":"v2","later_field":{"x":1}}`), &answer, 64); err != nil || !answer.Complete || len(answer.Decisions) != 1 {
		t.Fatalf("answer with an added field = %+v, %v", answer, err)
	}
	if err := strictjson.DecodeResponse([]byte(`{"complete":"yes"}`), &answer, 64); err == nil {
		t.Fatal("a wrong type for a known field was accepted")
	}
	if err := strictjson.DecodeResponse([]byte(`{"complete":true} {}`), &answer, 64); err == nil {
		t.Fatal("trailing data was accepted")
	}
}
