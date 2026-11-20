package edgehttp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type attestedTestAddr struct{ net.Addr }

func (attestedTestAddr) AttestedVisitor() {}

// A forwarded visitor's client address is the link-attested RemoteAddr: forwarding headers it sends are not read, even
// when that address is a trusted proxy's, where a direct visitor's headers would be.
func TestForwardIgnoresForwardingHeadersForClientIP(t *testing.T) {
	policy := policyFor(t, http.NotFoundHandler()) // trusts 10.0.0.0/8
	request := func(attested bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://app.example.test/", nil)
		r.RemoteAddr = "10.1.2.3:5555"
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Header.Set("X-Real-IP", "203.0.113.9")
		if attested {
			local := attestedTestAddr{&net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}}
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
		}
		return r
	}
	if got := policy.clientIP(request(false)); got != "203.0.113.9" {
		t.Fatalf("a direct visitor from a trusted proxy lost its forwarding header: %q", got)
	}
	if got := policy.clientIP(request(true)); got != "10.1.2.3" {
		t.Fatalf("an attested visitor's address was overridden by its headers: %q", got)
	}
}
