package edgehttp

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
)

// FreeRequestLimits is the request guard for publications a Free account owns (abuse guard; Paid
// plans have none beyond the edge-wide flood floor in ip_limits.go). Two token buckets per request:
// one per visitor address and publication, one per publication for all visitors together. The edge
// enforces only the rates the server sends (edgelimits.FreeRequests in the heartbeat answer); it
// starts with none, and the server sends none to a customer-owned edge. Prior art: zrok limits
// public shares to 2,000 requests per 5 minutes per IP; ngrok's rate-limit action keys on the client IP.
// Both tables reuse the address limiter's shards, idle sweep and key bound.
type FreeRequestLimits struct {
	tables atomic.Pointer[freeRequestTables]
}

type freeRequestTables struct {
	rates          edgelimits.FreeRequests
	perAddress     *addressLimiter // key: publication and visitor address
	perPublication *addressLimiter // key: publication
}

// NewFreeRequestLimits starts unlimited until the server's first answer carries rates.
func NewFreeRequestLimits() *FreeRequestLimits { return &FreeRequestLimits{} }

// Set applies the server's rates; the zero value removes the limit. Counters restart only when the rates changed.
func (l *FreeRequestLimits) Set(rates edgelimits.FreeRequests) {
	current := l.tables.Load()
	switch {
	case rates == (edgelimits.FreeRequests{}):
		l.tables.Store(nil)
	case current == nil || current.rates != rates:
		bucket := func(r edgelimits.Rate) addressRate {
			return addressRate{perSecond: r.PerSecond, burst: float64(r.Burst)}
		}
		l.tables.Store(&freeRequestTables{
			rates:          rates,
			perAddress:     newAddressLimiter(bucket(rates.PerAddress), addressRate{}, 0),
			perPublication: newAddressLimiter(bucket(rates.PerPublication), addressRate{}, 0),
		})
	}
}

// over takes one request of the publication from the visitor address's bucket, then the publication's, and returns the
// rates that refused it (nil when admitted or when the server sent none). The address goes first so one hammering
// visitor is refused by its own bucket without draining the publication's, which would turn one scraper into an
// outage for everyone else.
func (l *FreeRequestLimits) over(publication, address string) *edgelimits.FreeRequests {
	t := l.tables.Load()
	if t == nil {
		return nil
	}
	if _, ok := t.perAddress.admit(publication+"\x00"+addressKey(address), false); !ok {
		return &t.rates
	}
	if _, ok := t.perPublication.admit(publication, false); !ok {
		return &t.rates
	}
	return nil
}

// writeFreeRequestLimited answers a Free publication's visitor over a request rate. Unlike the edge-wide busy answer it
// says plainly that this is the Free plan's limit for public links, gives the numbers and links to pricing.
func writeFreeRequestLimited(w http.ResponseWriter, r *http.Request, rates *edgelimits.FreeRequests) {
	message := fmt.Sprintf("This is the Free plan's request limit for public links: %g requests per second for each visitor and %g per second for the whole link. Wait a moment and try again, or upgrade to remove the limit.", rates.PerAddress.PerSecond, rates.PerPublication.PerSecond)
	linkText := ""
	if rates.PricingURL != "" {
		linkText = "See plans and pricing"
	}
	writeRefusalLink(w, r, http.StatusTooManyRequests, "free_plan_rate_limited", "Free plan request limit", message, time.Second, linkText, rates.PricingURL)
}
