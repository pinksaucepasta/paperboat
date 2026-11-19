// Package edgelimits is the edge's byte pacing on public ingress: how fast one publication and
// one account may move bytes on one edge process. It shapes and never refuses: popularity is not
// abuse, so request and connection counts are not capped here (floods are stopped per client IP
// at the edge, and the plan's bandwidth allowance is the user-facing limit). The one exception is
// FreeRequests, the request rates of publications a Free account owns (abuse guard; Paid plans have
// none). It is one server configuration set sent to every edge in its heartbeat answer. Prior
// art: frp's per-proxy transport.bandwidthLimit; zrok computes per-account limits centrally and
// limits public shares to 2,000 requests per 5 minutes per IP; ngrok's rate-limit traffic-policy
// action keys on the client IP (conn.client_ip).
package edgelimits

import (
	"errors"
	"net/url"
)

// ErrInvalid reports a limit set an edge must not apply.
var ErrInvalid = errors.New("edge ingress limits invalid")

// minimumByteBurst is the edge's pacing chunk: a smaller burst could never pass one chunk.
const minimumByteBurst = 32 << 10

// Limit paces one key (a publication or an account) on one edge process.
type Limit struct {
	BytesPerSecond int64 `json:"bytes_per_second"`
	ByteBurst      int   `json:"byte_burst"`
}

// Rate is a token bucket: PerSecond sustained, Burst at once.
type Rate struct {
	PerSecond float64 `json:"per_second"`
	Burst     int     `json:"burst"`
}

// FreeRequests are the HTTP request rates of one publication owned by a Free account, on one edge
// process: per visitor address (an IPv4 address or IPv6 /64) and for all visitors together. The edge's
// flood floor per address (any number of links) stays on top of them. The server sends them to
// Paperboat-run edges only: the zero value, which a customer-owned edge is sent, means no limit,
// because the edge enforces only what it is told.
type FreeRequests struct {
	PerAddress     Rate `json:"per_address"`
	PerPublication Rate `json:"per_publication"`
	// PricingURL is where the 429 page sends a visitor to the plans; empty leaves the page without the link. The server
	// derives it from its configured site origin, so no edge knows a domain.
	PricingURL string `json:"pricing_url,omitempty"`
}

// IngressLimits is the whole set an edge applies.
type IngressLimits struct {
	Publication  Limit        `json:"publication"`
	Account      Limit        `json:"account"`
	FreeRequests FreeRequests `json:"free_requests"`
}

// Default is the set used until the server sends one, and the server's own default. One
// publication's byte rate stays above one stream's 150 Mbit/s target (PR-06).
func Default() IngressLimits {
	return IngressLimits{
		Publication: Limit{BytesPerSecond: 24 << 20, ByteBurst: 1 << 20},
		Account:     Limit{BytesPerSecond: 32 << 20, ByteBurst: 1 << 20},
		// An unbundled Vite or Next dev server fires 500-1,500 module requests on a first load (burst 1,500); a scraper holds 10/s.
		FreeRequests: FreeRequests{PerAddress: Rate{PerSecond: 10, Burst: 1500}, PerPublication: Rate{PerSecond: 50, Burst: 3000}},
	}
}

func (l Limit) validate() error {
	if l.BytesPerSecond < minimumByteBurst || l.BytesPerSecond > 100<<30 || l.ByteBurst < minimumByteBurst || l.ByteBurst > 1<<30 {
		return ErrInvalid
	}
	return nil
}

// Validate accepts the zero value (no limit) or two valid rates and an empty or https pricing link.
func (f FreeRequests) Validate() error {
	if f == (FreeRequests{}) {
		return nil
	}
	if u, err := url.Parse(f.PricingURL); err != nil || f.PricingURL != "" && (u.Scheme != "https" || u.Host == "") {
		return ErrInvalid
	}
	if f.PerAddress.validate() != nil || f.PerPublication.validate() != nil {
		return ErrInvalid
	}
	return nil
}

func (r Rate) validate() error {
	if r.PerSecond < 1 || r.PerSecond > 1e6 || r.Burst < 1 || r.Burst > 1e7 {
		return ErrInvalid
	}
	return nil
}

// Validate rejects a set with a missing, zero or absurd value, or an account rate below its
// publication's.
func (l IngressLimits) Validate() error {
	if l.FreeRequests.Validate() != nil {
		return ErrInvalid
	}
	if l.Publication.validate() != nil || l.Account.validate() != nil || l.Account.BytesPerSecond < l.Publication.BytesPerSecond {
		return ErrInvalid
	}
	return nil
}
