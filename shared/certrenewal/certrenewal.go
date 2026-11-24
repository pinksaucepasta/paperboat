// Package certrenewal decides when a public certificate is renewed and starts the order that replaces it. It is the
// one rule every component follows: the CA's suggested window (ARI, RFC 9773) when it offers one, otherwise a third of
// the lifetime left (half below ten days), which is Let's Encrypt's advice for lifetimes that keep getting shorter.
//
// Prior art: ~/ref/certmagic certificates.go currentlyInRenewalWindow and maintain.go updateARI; ARI and "replaces"
// come from acmez, the ACME library certmagic and Caddy use (golang.org/x/crypto/acme has neither).
package certrenewal

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"net/http"
	"sync"
	"time"

	acmez "github.com/mholt/acmez/v3/acme"
	"golang.org/x/crypto/acme"
)

const (
	shortLifetime = 10 * 24 * time.Hour
	// The CA is asked at least twice a day; its Retry-After is honoured inside these bounds.
	checkMin     = time.Hour
	checkDefault = 6 * time.Hour
	checkMax     = 12 * time.Hour
	fetchTimeout = 30 * time.Second
	userAgent    = "paperboat-certrenewal"
)

// Margin is how much of the certificate's lifetime is kept for renewing it when the CA suggests nothing.
func Margin(notBefore, notAfter time.Time) time.Duration {
	lifetime := notAfter.Sub(notBefore)
	if lifetime < shortLifetime {
		return lifetime / 2
	}
	return lifetime / 3
}

// spread is a stable point in [0, 1) for a certificate: a fleet issued together does not renew together, and the
// answer stays the same across restarts and repeated checks.
func spread(leaf *x509.Certificate) float64 {
	sum := sha256.Sum256(leaf.Raw)
	return float64(binary.BigEndian.Uint64(sum[:])>>11) / (1 << 53)
}

// Fallback is the renewal time without the CA's advice: the margin, up to a tenth of it earlier.
func Fallback(leaf *x509.Certificate) time.Time {
	margin := Margin(leaf.NotBefore, leaf.NotAfter)
	return leaf.NotAfter.Add(-margin - time.Duration(spread(leaf)*float64(margin)/10)).UTC().Truncate(time.Second)
}

// inWindow is the certificate's time inside the CA's window. A window ending too close to the expiry to survive
// failed attempts is cut off at half the margin.
func inWindow(leaf *x509.Certificate, start, end time.Time) time.Time {
	at := start.Add(time.Duration(spread(leaf) * float64(end.Sub(start))))
	if latest := leaf.NotAfter.Add(-Margin(leaf.NotBefore, leaf.NotAfter) / 2); at.After(latest) {
		at = latest
	}
	return at.UTC().Truncate(time.Second)
}

// RetryDelay is the wait after the attempt-th failed order in a row: 1, 4 and 16 minutes, then an hour. That is four
// orders in the first hour, under Let's Encrypt's five failed validations per name and hour.
func RetryDelay(attempt int) time.Duration {
	delay := time.Minute
	for i := 1; i < attempt && delay < time.Hour; i++ {
		delay *= 4
	}
	return min(delay, time.Hour)
}

// Leaf is the first certificate of a PEM chain.
func Leaf(chain []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(chain)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate in the PEM file")
	}
	return x509.ParseCertificate(block.Bytes)
}

// Schedule answers when certificates of one CA are due. A nil Schedule knows no CA and always uses the fallback.
type Schedule struct {
	client *acmez.Client

	mu      sync.Mutex
	entries map[[32]byte]Plan
}

// Plan is a certificate's renewal time and when to ask the CA again. FromCA is false while the CA offers nothing.
type Plan struct {
	RenewAt time.Time
	CheckAt time.Time
	FromCA  bool
}

func New(directoryURL string, client *http.Client) *Schedule {
	if directoryURL == "" {
		return nil
	}
	return &Schedule{client: &acmez.Client{Directory: directoryURL, HTTPClient: client, UserAgent: userAgent}, entries: map[[32]byte]Plan{}}
}

// Plan is the certificate's current plan; the CA is asked only when its previous answer is due for a check.
func (s *Schedule) Plan(ctx context.Context, leaf *x509.Certificate, now time.Time) Plan {
	if s == nil {
		return Plan{RenewAt: Fallback(leaf), CheckAt: now.Add(checkMax)}
	}
	id := sha256.Sum256(leaf.Raw)
	s.mu.Lock()
	for known, plan := range s.entries { // a replaced certificate is not asked about again
		if known != id && now.Sub(plan.CheckAt) > 2*checkMax {
			delete(s.entries, known)
		}
	}
	plan, known := s.entries[id]
	s.mu.Unlock()
	if known && now.Before(plan.CheckAt) {
		return plan
	}
	if !plan.FromCA {
		plan.RenewAt = Fallback(leaf)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	info, err := s.client.GetRenewalInfo(fetchCtx, leaf)
	cancel()
	switch {
	case err == nil:
		wait := checkDefault
		if info.RetryAfter != nil {
			wait = min(max(time.Until(*info.RetryAfter), checkMin), checkMax)
		}
		plan = Plan{RenewAt: inWindow(leaf, info.SuggestedWindow.Start, info.SuggestedWindow.End), CheckAt: now.Add(wait), FromCA: true}
	case errors.Is(err, acmez.ErrUnsupported):
		plan = Plan{RenewAt: Fallback(leaf), CheckAt: now.Add(checkMax)}
	default: // the last answer stands until the CA answers again
		plan.CheckAt = now.Add(checkMin)
	}
	if ctx.Err() == nil {
		s.mu.Lock()
		s.entries[id] = plan
		s.mu.Unlock()
	}
	return plan
}

// AuthorizeOrder starts an order. One that replaces a certificate says so (ARI "replaces"), which exempts it from the
// CA's rate limits; a CA that refuses the replacement gets a plain order, like certmagic (acmeissuer.go doIssue).
func AuthorizeOrder(ctx context.Context, client *acme.Client, ids []acme.AuthzID, replaces *x509.Certificate, profile string) (*acme.Order, error) {
	if replaces != nil {
		if order, err := replacementOrder(ctx, client, ids, replaces, profile); err == nil {
			return order, nil
		} else if ctx.Err() != nil {
			return nil, err
		}
	}
	var options []acme.OrderOption
	if profile != "" {
		options = append(options, acme.WithOrderIssuanceProfile(acme.ProfileName(profile)))
	}
	return client.AuthorizeOrder(ctx, ids, options...)
}

func replacementOrder(ctx context.Context, client *acme.Client, ids []acme.AuthzID, replaces *x509.Certificate, profile string) (*acme.Order, error) {
	id, err := acmez.ARIUniqueIdentifier(replaces)
	if err != nil {
		return nil, err
	}
	if client.KID == "" {
		return nil, errors.New("the ACME account is not registered")
	}
	ordering := &acmez.Client{Directory: client.DirectoryURL, HTTPClient: client.HTTPClient, UserAgent: client.UserAgent}
	directory, err := ordering.GetDirectory(ctx)
	if err != nil {
		return nil, err
	}
	if directory.RenewalInfo == "" {
		return nil, acmez.ErrUnsupported
	}
	request := acmez.Order{Replaces: id, Profile: profile}
	for _, authz := range ids {
		request.Identifiers = append(request.Identifiers, acmez.Identifier{Type: authz.Type, Value: authz.Value})
	}
	order, err := ordering.NewOrder(ctx, acmez.Account{Location: string(client.KID), PrivateKey: client.Key}, request)
	if err != nil {
		return nil, err
	}
	return client.GetOrder(ctx, order.Location)
}
