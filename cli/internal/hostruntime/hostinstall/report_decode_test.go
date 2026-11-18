package hostinstall

import (
	"errors"
	"strings"
	"testing"
)

// A candidate's report and descriptor are another build's output: a later build's extra fields are fine,
// but a value cut at the limit, followed by anything, or oversized is refused (fail closed).
func TestDecodeBoundedAcceptsOneValueAndIgnoresUnknownFields(t *testing.T) {
	var got struct{ A string }
	if err := decodeBounded([]byte(`{"A":"x","Later":{"field":1}}`+"\n"), 64, &got); err != nil || got.A != "x" {
		t.Fatalf("one value with an unknown field: %+v %v", got, err)
	}
	for name, body := range map[string]string{
		"trailing garbage": `{"A":"x"} {"A":"y"}`,
		"trailing text":    `{"A":"x"} warning: something`,
		"cut":              `{"A":"x`,
		"empty":            ``,
	} {
		if err := decodeBounded([]byte(body), 64, &got); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := decodeBounded([]byte(`{"A":"`+strings.Repeat("x", 100)+`"}`), 64, &got); !errors.Is(err, errCandidateReportTooLarge) {
		t.Fatalf("oversized report: %v", err)
	}
}

func TestDecodeCarrierIgnoresFieldsALaterBuildAdded(t *testing.T) {
	body := []byte(`{"Instance":"i","Generation":"g","SomeLaterField":true}`)
	c, err := decodeCarrier(body)
	if err != nil || c.Instance != "i" || c.Generation != "g" {
		t.Fatalf("a later build's descriptor was refused: %+v %v", c, err)
	}
}
