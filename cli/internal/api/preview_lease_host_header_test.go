package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPreviewLeaseTargetHostHeader(t *testing.T) {
	for _, test := range []struct {
		target PreviewLeaseTarget
		ok     bool
	}{
		{PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: "app.example.test"}, true},
		{PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, true},
		{PreviewLeaseTarget{Scheme: "unix", Address: "/tmp/app.sock", HostHeader: "app.example.test"}, false},
		{PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: "app.example.test:8080"}, false},
		{PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000", HostHeader: strings.Repeat("a", 254)}, false},
	} {
		if err := validatePreviewLeaseTarget(test.target); (err == nil) != test.ok {
			t.Errorf("%+v: err = %v, want ok = %v", test.target, err, test.ok)
		}
	}
	// omitted when unset, so a target without one is byte-identical to what servers already accept
	raw, _ := json.Marshal(PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"})
	if strings.Contains(string(raw), "host_header") {
		t.Fatalf("empty host header serialized: %s", raw)
	}
}
