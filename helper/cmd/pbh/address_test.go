package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicAddressUsesConfiguredEndpoint(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"address", `{"address":"192.0.2.10"}`, http.StatusOK, "192.0.2.10"},
		{"server unavailable", `{"address":"192.0.2.10"}`, http.StatusServiceUnavailable, ""},
		{"invalid address", `{"address":"invalid"}`, http.StatusOK, ""},
		{"malformed response", `{"address":`, http.StatusOK, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Method != http.MethodGet || r.URL.Path != "/selfhost/address" {
					t.Errorf("unexpected address request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			t.Setenv("PAPERBOAT_SELFHOST_ADDRESS_URL", server.URL+"/selfhost/address")
			address, err := publicAddress(context.Background())
			if !called || address != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("called=%t address=%q error=%v", called, address, err)
			}
		})
	}
}

func TestPublicAddressMissingConfigurationExplainsEndpointFlag(t *testing.T) {
	t.Setenv("PAPERBOAT_SELFHOST_ADDRESS_URL", "")
	_, err := publicAddress(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--endpoint-host") {
		t.Fatalf("missing endpoint guidance: %v", err)
	}
}
