package hostruntimecmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodeBrowserDomainNormalizesDomain(t *testing.T) {
	request, err := decodeBrowserDomain(strings.NewReader(`{"owner":"1000","domain":"Apps.Local.Example.Test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.Owner != "1000" {
		t.Fatalf("owner = %q, want %q", request.Owner, "1000")
	}
	if request.Domain != "apps.local.example.test" {
		t.Fatalf("domain = %q, want %q", request.Domain, "apps.local.example.test")
	}
}

func TestDecodeBrowserDomainRejectsMissingOrInvalidOwner(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
	}{
		{
			name:    "missing",
			payload: `{"domain":"apps.local.example.test"}`,
		},
		{
			name:    "blank",
			payload: `{"owner":" \t ","domain":"apps.local.example.test"}`,
		},
		{
			name:    "too long",
			payload: `{"owner":"` + strings.Repeat("x", 185) + `","domain":"apps.local.example.test"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeBrowserDomain(strings.NewReader(test.payload)); err == nil {
				t.Fatal("accepted browser-domain request with invalid owner")
			}
		})
	}
}

func TestDecodeBrowserDomainRejectsTrailingJSON(t *testing.T) {
	payload := `{"owner":"1000","domain":"apps.local.example.test"} {"owner":"1001","domain":"apps.local.example.test"}`
	if _, err := decodeBrowserDomain(strings.NewReader(payload)); err == nil {
		t.Fatal("accepted a second JSON value after the browser-domain request")
	}
}

func TestDecodeBrowserDomainEnforcesPayloadLimit(t *testing.T) {
	const maxPayloadBytes = 4096
	request := []byte(`{"owner":"1000","domain":"apps.local.example.test"}`)
	if len(request) >= maxPayloadBytes {
		t.Fatal("test request unexpectedly exceeds the payload limit")
	}

	limitPayload := append(append([]byte(nil), request...), bytes.Repeat([]byte(" "), maxPayloadBytes-len(request))...)
	if len(limitPayload) != maxPayloadBytes {
		t.Fatalf("boundary payload size = %d, want %d", len(limitPayload), maxPayloadBytes)
	}
	if _, err := decodeBrowserDomain(bytes.NewReader(limitPayload)); err != nil {
		t.Fatalf("rejected payload at %d-byte limit: %v", maxPayloadBytes, err)
	}

	oversizedPayload := append(limitPayload, ' ')
	if len(oversizedPayload) != maxPayloadBytes+1 {
		t.Fatalf("oversized payload size = %d, want %d", len(oversizedPayload), maxPayloadBytes+1)
	}
	if _, err := decodeBrowserDomain(bytes.NewReader(oversizedPayload)); err == nil {
		t.Fatal("accepted browser-domain payload larger than the limit")
	}
}
