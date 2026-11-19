// Package capver is the one capability version every Paperboat binary (pb, pbh, edge, relay) sends
// to the server on each HTTP request, so the server can tailor an answer to what its sender
// understands (prior art: Tailscale tailcfg.CapabilityVersion, ~/ref/tailscale/tailcfg/tailcfg.go).
package capver

import (
	"net/http"
	"strconv"
)

// Version rises by one whenever any binary starts to understand something new from the server, or
// starts to send something new. It is never lowered or reused. Log each bump here:
//
//	1: 2026-11-18, the launch release.
//	2: 2026-11-19, signed peer documents carry home_relay (peernetwork minCapVersion); a build that sends 1 does not decode it.
const Version = 2

// Header carries Version. A request without it comes from a build older than this package.
const Header = "X-Paperboat-Capability-Version"

// Transport sets Header on every request that base sends. base nil means the standard default transport.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		//paperboat:allow-source-policy default-http owner=capver reason=nil-base-means-the-standard-transport
		base = http.DefaultTransport
	}
	return transport{base: base}
}

type transport struct{ base http.RoundTripper }

func (t transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Header.Get(Header) == "" {
		request = request.Clone(request.Context()) // a RoundTripper must not modify the caller's request
		request.Header.Set(Header, strconv.Itoa(Version))
	}
	return t.base.RoundTrip(request)
}

// FromRequest returns the sender's capability version, or 0 for a sender older than this package
// or a malformed value.
func FromRequest(request *http.Request) int {
	value, err := strconv.Atoi(request.Header.Get(Header))
	if err != nil || value < 0 {
		return 0
	}
	return value
}
