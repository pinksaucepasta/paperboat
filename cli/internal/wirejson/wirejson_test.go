package wirejson

import (
	"errors"
	"os"
	"strings"
	"testing"
)

type document struct {
	Name  string            `json:"name"`
	Inner struct{ A int }   `json:"inner"`
	List  []struct{ B int } `json:"list"`
}

func TestDecodeIgnoresUnknownFieldsAtEveryLevel(t *testing.T) {
	var out document
	raw := `{"name":"x","new_field":{"a":1},"inner":{"A":2,"extra":true},"list":[{"B":3,"extra":"y"}]}`
	if err := Decode([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "x" || out.Inner.A != 2 || len(out.List) != 1 || out.List[0].B != 3 {
		t.Fatalf("decoded %+v", out)
	}
}

func TestDecodeStillRejectsWrongTypesAndTrailingData(t *testing.T) {
	var out document
	if err := Decode([]byte(`{"name":5}`), &out); err == nil {
		t.Fatal("wrong field type accepted")
	}
	if err := Decode([]byte(`{"name":"x"} {"name":"y"}`), &out); !errors.Is(err, ErrTrailingData) {
		t.Fatalf("trailing data: %v", err)
	}
}

// Documents received from the control plane are decoded tolerantly: none of these server-response
// decoders may reject unknown fields again (local state files and local IPC stay strict).
func TestServerResponseDecodersDoNotRejectUnknownFields(t *testing.T) {
	for _, path := range []string{
		"../hostruntime/machinecontrol/source.go",
		"../hostruntime/enrollment/client.go",
		"../hostruntime/preview/attachment_validate.go",
		"../hostruntime/preview/readiness_observer.go",
		"../hostruntime/preview/accessor_discovery.go",
		"../hostruntime/preview/private_access.go",
		"../hostruntime/tunnelenrollment/client.go",
		"../hostruntime/tunnelenrollment/bootstrap_descriptor.go",
		"../hostruntime/peeridentity/enroll.go",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "DisallowUnknownFields") {
			t.Errorf("%s rejects unknown fields in a server response; use wirejson so a newer server never breaks this client", path)
		}
	}
}
