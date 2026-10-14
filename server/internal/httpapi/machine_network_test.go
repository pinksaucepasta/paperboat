package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestNetworkUsesForwardedAddressOnlyFromTrustedProxy(t *testing.T) {
	resolve := newRequestNetwork([]string{"10.0.0.0/8", "2001:db8::/32"})
	tests := []struct {
		name, remote, forwarded, want string
	}{
		{name: "forwarded chain", remote: "10.1.2.3:443", forwarded: "198.51.100.21, 10.2.3.4", want: "198.51.100.21"},
		{name: "untrusted peer cannot spoof", remote: "203.0.113.9:443", forwarded: "198.51.100.23", want: "203.0.113.9"},
		{name: "malformed trusted headers fall back", remote: "10.1.2.3:443", forwarded: "also-bad", want: "10.1.2.3"},
		{name: "trusted ipv6 proxy", remote: "[2001:db8::1]:443", forwarded: "2001:4860::1", want: "2001:4860::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/auth/device/authorize", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", tc.forwarded)
			if got := resolve(req); got != tc.want {
				t.Fatalf("network = %q, want %q", got, tc.want)
			}
			if tc.name == "untrusted peer cannot spoof" && req.Header.Get("X-Forwarded-For") != "" {
				t.Fatalf("untrusted forwarding headers retained: %v", req.Header)
			}
		})
	}
}

func TestTrustedClientNetworkSanitizesBeforeHandler(t *testing.T) {
	var network string
	var forwarded string
	handler := trustedClientNetwork([]string{"10.0.0.0/8"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network = resolvedRequestNetwork(r)
		forwarded = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest("GET", "/healthz", nil)
	request.RemoteAddr = "203.0.113.8:443"
	request.Header.Set("X-Forwarded-For", "198.51.100.7")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if network != "203.0.113.8" || forwarded != "" {
		t.Fatalf("network=%q forwarded=%q", network, forwarded)
	}
}

func TestRequestNetworkDefaultsToDirectPeer(t *testing.T) {
	resolve := newRequestNetwork(nil)
	req := httptest.NewRequest("POST", "/v1/auth/device/token", nil)
	req.RemoteAddr = "191.0.2.10:1234"
	if got := resolve(req); got != "191.0.2.10" {
		t.Fatalf("network = %q", got)
	}
}
