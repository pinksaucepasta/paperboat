package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/tlscert"
)

var (
	ErrCertificateSelectorInvalid = errors.New("invalid certificate selector")
	ErrCertificateSelectorBusy    = errors.New("certificate selector is busy")
)

const maximumCertificateDemandCalls = 1024

type CertificateSource interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}
type OnDemandCertificateRequester interface {
	RequestCertificate(context.Context, string) error
}
type OnDemandCertificateMatcher interface{ RequiresExactCertificate(string) bool }
type OnDemandCertificateRequesterFunc func(context.Context, string) error

func (f OnDemandCertificateRequesterFunc) RequestCertificate(ctx context.Context, hostname string) error {
	return f(ctx, hostname)
}

type OnDemandCertificateMatcherFunc func(string) bool

func (f OnDemandCertificateMatcherFunc) RequiresExactCertificate(hostname string) bool {
	return f(hostname)
}

type CertificateSelectorConfig struct {
	Source            CertificateSource
	OnDemandRequester OnDemandCertificateRequester
	OnDemandMatcher   OnDemandCertificateMatcher
	// Wildcard keeps TLS up for any name it covers that has no certificate of its own (a stopped,
	// unavailable or unknown preview), so the edge can answer with its branded error page.
	Wildcard interface {
		WildcardCertificate(string) (*tls.Certificate, error)
	}
	HandshakeTimeout time.Duration
}

type certificateDemandCall struct {
	done       chan struct{}
	err        error
	finishedAt time.Time
}
type CertificateSelector struct {
	source   CertificateSource
	onDemand OnDemandCertificateRequester
	matcher  OnDemandCertificateMatcher
	wildcard interface {
		WildcardCertificate(string) (*tls.Certificate, error)
	}
	timeout time.Duration
	mu      sync.Mutex
	demand  map[string]*certificateDemandCall
}

func NewCertificateSelector(config CertificateSelectorConfig) (*CertificateSelector, error) {
	if config.Source == nil || config.HandshakeTimeout < 0 || config.HandshakeTimeout > 30*time.Second {
		return nil, ErrCertificateSelectorInvalid
	}
	if config.HandshakeTimeout == 0 {
		config.HandshakeTimeout = 2 * time.Second
	}
	return &CertificateSelector{source: config.Source, onDemand: config.OnDemandRequester, matcher: config.OnDemandMatcher, wildcard: config.Wildcard, timeout: config.HandshakeTimeout, demand: make(map[string]*certificateDemandCall)}, nil
}

func (s *CertificateSelector) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello == nil {
			return nil, tlscert.ErrCertificateMissing
		}
		ctx, cancel := context.WithTimeout(hello.Context(), s.timeout)
		defer cancel()
		return s.lookup(ctx, hello.ServerName)
	}}
}

// WildcardTLSConfig serves only the platform wildcard for a covered name and never asks the server for an
// on-demand certificate: the edge's not-found page for stopped or unknown names must not turn every
// handshake (scanners included) into a control request.
func (s *CertificateSelector) WildcardTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello == nil || s.wildcard == nil {
			return nil, tlscert.ErrCertificateMissing
		}
		canonical, wildcard, err := tlscert.NormalizeHostname(hello.ServerName)
		if err != nil || wildcard {
			return nil, tlscert.ErrCertificateMissing
		}
		certificate, err := s.wildcard.WildcardCertificate(canonical)
		if err != nil || !certificateCoversHost(certificate, canonical) {
			return nil, tlscert.ErrCertificateMissing
		}
		return certificate, nil
	}}
}

func (s *CertificateSelector) lookup(ctx context.Context, hostname string) (*tls.Certificate, error) {
	certificate, err := s.lookupExact(ctx, hostname)
	if err == nil || s.wildcard == nil {
		return certificate, err
	}
	// A stopped, expired or unknown preview under the platform wildcard still completes TLS, so the edge can
	// show its branded "stopped or does not exist" page instead of dropping the handshake (ngrok and Cloudflare
	// answer such names with an error page too). The wildcard already covers the name; nothing new is exposed.
	if canonical, wildcard, normalizeErr := tlscert.NormalizeHostname(hostname); normalizeErr == nil && !wildcard {
		if fallback, fallbackErr := s.wildcard.WildcardCertificate(canonical); fallbackErr == nil && certificateCoversHost(fallback, canonical) {
			return fallback, nil
		}
	}
	return nil, err
}

func (s *CertificateSelector) lookupExact(ctx context.Context, hostname string) (*tls.Certificate, error) {
	canonical, wildcard, err := tlscert.NormalizeHostname(hostname)
	if err != nil || wildcard {
		return nil, tlscert.ErrCertificateMissing
	}
	hostname = canonical
	exact := s.matcher != nil && s.matcher.RequiresExactCertificate(hostname)
	certificate, sourceErr := s.source.GetCertificate(&tls.ClientHelloInfo{ServerName: hostname})
	if sourceErr == nil && certificate != nil && ((!exact && certificateCoversHost(certificate, hostname)) || (exact && certificateCoversExactHost(certificate, hostname))) {
		return certificate, nil
	}
	if sourceErr != nil && !errors.Is(sourceErr, tlscert.ErrCertificateMissing) && !errors.Is(sourceErr, tlscert.ErrCertificateExpired) {
		return nil, sourceErr
	}
	if s.onDemand == nil {
		if sourceErr != nil {
			return nil, sourceErr
		}
		return nil, tlscert.ErrCertificateMissing
	}
	if err := s.request(ctx, hostname); err != nil {
		return nil, err
	}
	certificate, err = s.source.GetCertificate(&tls.ClientHelloInfo{ServerName: hostname})
	if err != nil {
		return nil, err
	}
	if exact && !certificateCoversExactHost(certificate, hostname) || !exact && !certificateCoversHost(certificate, hostname) {
		return nil, tlscert.ErrCertificateMissing
	}
	return certificate, nil
}

func (s *CertificateSelector) request(ctx context.Context, hostname string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	s.mu.Lock()
	for key, call := range s.demand {
		if call == nil || !call.finishedAt.IsZero() && now.Sub(call.finishedAt) >= s.timeout {
			delete(s.demand, key)
		}
	}
	if call := s.demand[hostname]; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if len(s.demand) >= maximumCertificateDemandCalls {
		s.mu.Unlock()
		return ErrCertificateSelectorBusy
	}
	call := &certificateDemandCall{done: make(chan struct{})}
	s.demand[hostname] = call
	s.mu.Unlock()
	requestCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	err := s.onDemand.RequestCertificate(requestCtx, hostname)
	cancel()
	s.mu.Lock()
	call.err = err
	if err == nil {
		call.finishedAt = time.Now()
	} else {
		delete(s.demand, hostname)
	}
	close(call.done)
	s.mu.Unlock()
	return err
}

func certificateCoversHost(certificate *tls.Certificate, hostname string) bool {
	leaf, ok := certificateLeaf(certificate)
	return ok && leaf.VerifyHostname(hostname) == nil
}
func certificateLeaf(certificate *tls.Certificate) (*x509.Certificate, bool) {
	if certificate == nil || len(certificate.Certificate) == 0 {
		return nil, false
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	return leaf, err == nil
}
func certificateCoversExactHost(certificate *tls.Certificate, hostname string) bool {
	if !certificateCoversHost(certificate, hostname) {
		return false
	}
	leaf, ok := certificateLeaf(certificate)
	if !ok {
		return false
	}
	for _, name := range leaf.DNSNames {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "*.") {
			return false
		}
	}
	return true
}
