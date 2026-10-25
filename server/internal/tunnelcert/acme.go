package tunnelcert

// This file contains the server-owned RFC 8555 client.  It intentionally
// stops at the authenticated issuer and DNS-provider boundaries: account
// signing keys and DNS API tokens are resolved by reference for one request,
// kept in memory, and never copied into a database row, wire view, audit event,
// or error string.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

const (
	defaultACMETimeout          = 2 * time.Minute
	defaultACMEPropagation      = 2 * time.Minute
	defaultACMECleanup          = 30 * time.Second
	defaultACMEPollInterval     = 2 * time.Second
	defaultACMERetryBase        = 500 * time.Millisecond
	defaultACMERetryMax         = 10 * time.Second
	maxACMEChainCertificates    = 16
	maxACMECertificateDERBytes  = 16 << 20
	maxACMEDNSRecordValueLength = 512
)

// SignerReferenceSource resolves a server-side ACME account key by an
// opaque reference.  Implementations must not persist or expose the returned
// key and should return a fresh in-memory signer when practical.
type SignerReferenceSource interface {
	ResolveSigner(context.Context, string) (crypto.Signer, error)
}

// DNS01Record is the only DNS material passed to a provider. ProviderID is
// opaque provider state and must not contain credentials.
type DNS01Record struct {
	// Domain is the ACME authorization name (the requested customer domain).
	Domain string
	// Name is the provider write/read target. It is the delegated target when
	// TargetName differs from AuthorizationName.
	Name              string
	Value             string
	TTL               time.Duration
	ProviderID        string
	AuthorizationName string
	TargetName        string
}

// DNS01Provider owns the complete DNS-01 record lifecycle. Present and
// Cleanup must be idempotent for the same ProviderID. Wait must not return
// success until the expected TXT value is visible through authoritative or
// configured recursive resolvers.
type DNS01Provider interface {
	Present(context.Context, DNS01Record) (DNS01Record, error)
	Wait(context.Context, DNS01Record) error
	Cleanup(context.Context, DNS01Record) error
}

type ACMEIssuerConfig struct {
	DirectoryURL string
	AccountKID   string
	AccountEmail string

	// AccountKey is an explicitly injected in-memory signer for tests or a
	// process-local secret provider. Production composition should use
	// AccountKeys and AccountKeyReference instead.
	AccountKey          crypto.Signer
	AccountKeys         SignerReferenceSource
	AccountKeyReference string

	DNS DNS01Provider
	// HTTP01, when set, answers http-01 authorizations instead of DNS-01. It
	// is a staging path: configuration refuses it in production. Exactly one
	// of DNS and HTTP01 must be set.
	HTTP01             HTTP01Publisher
	HTTPClient         *http.Client
	Timeout            time.Duration
	PropagationTimeout time.Duration
	CleanupTimeout     time.Duration
	PollInterval       time.Duration
	MaxAttempts        int
	RetryBase          time.Duration
	RetryMax           time.Duration
	Issuer             string
	// ChallengeZone is a Paperboat-owned authoritative DNS zone for user-owned
	// delegated targets. Platform wildcards write directly to their
	// server-owned _acme-challenge name and do not require a CNAME in this zone.
	ChallengeZone string
}

type ACMEIssuer struct {
	directoryURL       string
	accountKID         acme.KeyID
	accountEmail       string
	accountKey         crypto.Signer
	accountKeys        SignerReferenceSource
	accountKeyRef      string
	dns                DNS01Provider
	http01             HTTP01Publisher
	httpClient         *http.Client
	timeout            time.Duration
	propagationTimeout time.Duration
	cleanupTimeout     time.Duration
	pollInterval       time.Duration
	maxAttempts        int
	retryBase          time.Duration
	retryMax           time.Duration
	issuer             string
	challengeZone      string
	accountMu          sync.Mutex
}

func NewACMEIssuer(config ACMEIssuerConfig) (*ACMEIssuer, error) {
	parsed, err := url.Parse(config.DirectoryURL)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: ACME directory URL is invalid", ErrInvalid)
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("%w: insecure ACME directory must be loopback", ErrInvalid)
		}
	}
	if config.AccountKey == nil {
		if config.AccountKeys == nil || !validKeyReference(config.AccountKeyReference) {
			return nil, fmt.Errorf("%w: exactly one ACME account-key source is required", ErrInvalid)
		}
	} else if config.AccountKeys != nil {
		return nil, fmt.Errorf("%w: exactly one ACME account-key source is required", ErrInvalid)
	}
	if config.AccountKey != nil && !supportedAccountKey(config.AccountKey) {
		return nil, fmt.Errorf("%w: ACME account key algorithm is unsupported", ErrInvalid)
	}
	if config.DNS != nil && config.HTTP01 != nil {
		return nil, fmt.Errorf("%w: exactly one challenge provider (DNS-01 or HTTP-01) is allowed", ErrInvalid)
	}
	if config.DNS == nil && config.HTTP01 == nil {
		return nil, fmt.Errorf("%w: DNS-01 provider is required", ErrDNSChallengeUnavailable)
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	} else {
		clone := *client
		client = &clone
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrIssuerUnavailable }
	timeout := config.Timeout
	if timeout <= 0 || timeout > 10*time.Minute {
		timeout = defaultACMETimeout
	}
	client.Timeout = timeout
	propagationTimeout := config.PropagationTimeout
	if propagationTimeout <= 0 || propagationTimeout > 10*time.Minute {
		propagationTimeout = defaultACMEPropagation
	}
	cleanupTimeout := config.CleanupTimeout
	if cleanupTimeout <= 0 || cleanupTimeout > 2*time.Minute {
		cleanupTimeout = defaultACMECleanup
	}
	pollInterval := config.PollInterval
	if pollInterval <= 0 || pollInterval > time.Minute {
		pollInterval = defaultACMEPollInterval
	}
	maxAttempts := config.MaxAttempts
	if maxAttempts <= 0 || maxAttempts > 8 {
		maxAttempts = 5
	}
	retryBase := config.RetryBase
	if retryBase <= 0 || retryBase > time.Minute {
		retryBase = defaultACMERetryBase
	}
	retryMax := config.RetryMax
	if retryMax <= 0 || retryMax > 2*time.Minute {
		retryMax = defaultACMERetryMax
	}
	issuer := strings.TrimSpace(config.Issuer)
	if issuer == "" {
		issuer = "letsencrypt"
	}
	if !validMetadata(issuer, 256) {
		return nil, fmt.Errorf("%w: ACME issuer is invalid", ErrInvalid)
	}
	challengeZone := strings.TrimSpace(config.ChallengeZone)
	if challengeZone != "" {
		if _, wildcard, zoneErr := normalizeHostname(challengeZone); zoneErr != nil || wildcard {
			return nil, fmt.Errorf("%w: challenge zone is invalid", ErrInvalid)
		}
	}
	return &ACMEIssuer{directoryURL: strings.TrimRight(config.DirectoryURL, "/"), accountKID: acme.KeyID(strings.TrimSpace(config.AccountKID)), accountEmail: strings.TrimSpace(config.AccountEmail), accountKey: config.AccountKey, accountKeys: config.AccountKeys, accountKeyRef: config.AccountKeyReference, dns: config.DNS, http01: config.HTTP01, httpClient: client, timeout: timeout, propagationTimeout: propagationTimeout, cleanupTimeout: cleanupTimeout, pollInterval: pollInterval, maxAttempts: maxAttempts, retryBase: retryBase, retryMax: retryMax, issuer: issuer, challengeZone: challengeZone}, nil
}

func (i *ACMEIssuer) Issue(ctx context.Context, request IssueRequest) (CertificateBundle, error) {
	if i == nil {
		return CertificateBundle{}, ErrIssuerUnavailable
	}
	if request.Domain.Strategy != StrategyDelegatedDNS01 && request.Domain.Strategy != StrategyPlatformDNS01 && request.Domain.Strategy != StrategyOnDemandLeaf && request.Domain.Strategy != StrategyWildcard {
		return CertificateBundle{}, fmt.Errorf("%w: ACME issuer only supports DNS-01 strategies", ErrIssuerUnavailable)
	}
	return i.IssueDNS01(ctx, request.Domain)
}

// IssueLeaf is the bounded exact-SNI path used for an on-demand request under
// a verified wildcard binding. The wildcard remains the ownership boundary,
// while the ACME order and returned certificate contain only the requested
// one-label hostname.
func (i *ACMEIssuer) IssueLeaf(ctx context.Context, request IssueRequest) (CertificateBundle, error) {
	if i == nil {
		return CertificateBundle{}, ErrIssuerUnavailable
	}
	if request.Domain.Strategy != StrategyOnDemandLeaf {
		return CertificateBundle{}, fmt.Errorf("%w: exact leaf strategy is invalid", ErrInvalid)
	}
	bound, wildcard, err := normalizeHostname(request.Domain.Hostname)
	if err != nil || !wildcard {
		return CertificateBundle{}, fmt.Errorf("%w: exact leaf requires a wildcard binding", ErrInvalid)
	}
	leaf := request.LeafHostname
	if leaf == "" {
		return CertificateBundle{}, fmt.Errorf("%w: exact leaf hostname is required", ErrInvalid)
	}
	leaf, leafWildcard, err := normalizeHostname(leaf)
	if err != nil || leafWildcard || !oneLabelUnderWildcard(leaf, bound) {
		return CertificateBundle{}, fmt.Errorf("%w: exact leaf hostname is outside the wildcard binding", ErrInvalid)
	}
	domain := request.Domain
	domain.Hostname = leaf
	domain.LeafHostname = ""
	domain.Strategy = StrategyDelegatedDNS01
	domain.AllowOnDemandFallback = false
	return i.IssueDNS01(ctx, domain)
}

// Revoke asks the configured ACME authority to revoke the leaf certificate.
// The PEM is consumed in memory only; callers should remove their own bundle
// immediately after this call returns. The account signer is resolved by
// reference in client and is never included in the returned error.
func (i *ACMEIssuer) Revoke(ctx context.Context, certificatePEM []byte) error {
	ctx = certificateContext(ctx)
	certificate, err := parseACMECertificate(certificatePEM)
	if err != nil {
		return err
	}
	client, err := i.client(ctx)
	if err != nil {
		return err
	}
	if err := client.RevokeCert(ctx, client.Key, certificate.Raw, acme.CRLReasonUnspecified); err != nil {
		return mapACMEError(err)
	}
	return nil
}

// RevokeBundle is the stronger revocation path for authorities that only
// accept the certificate's own key. The account-key attempt remains first so
// the normal RFC 8555 authorization is used whenever supported; the leaf key
// is consumed only in memory as a bounded fallback.
func (i *ACMEIssuer) RevokeBundle(ctx context.Context, bundle CertificateBundle) error {
	ctx = certificateContext(ctx)
	certificate, err := parseACMECertificate(bundle.CertificatePEM)
	if err != nil {
		return err
	}
	if len(bundle.PrivateKeyPEM) == 0 || len(bundle.PrivateKeyPEM) > maxACMECertificateDERBytes {
		return ErrCertificateInvalid
	}
	leafKey, err := parsePrivateKey(bundle.PrivateKeyPEM)
	if err != nil {
		return ErrCertificateInvalid
	}
	leafSigner, ok := leafKey.(crypto.Signer)
	if !ok {
		return ErrCertificateInvalid
	}
	client, err := i.client(ctx)
	if err != nil {
		return err
	}
	accountErr := client.RevokeCert(ctx, client.Key, certificate.Raw, acme.CRLReasonUnspecified)
	if accountErr == nil {
		return nil
	}
	if err := client.RevokeCert(ctx, leafSigner, certificate.Raw, acme.CRLReasonUnspecified); err != nil {
		return mapACMEError(accountErr)
	}
	return nil
}

func parseACMECertificate(certificatePEM []byte) (*x509.Certificate, error) {
	if len(certificatePEM) == 0 || len(certificatePEM) > maxACMECertificateDERBytes {
		return nil, ErrCertificateInvalid
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, ErrCertificateInvalid
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || certificate == nil {
		return nil, ErrCertificateInvalid
	}
	return certificate, nil
}

// IssueDNS01 performs an RFC 8555 order, presents one or more DNS records,
// waits for propagation, accepts each DNS-01 challenge, finalizes the order,
// and returns a validated in-memory bundle. Every presented record is cleaned
// up on success, cancellation, authorization failure, and certificate parse
// failure.
func (i *ACMEIssuer) IssueDNS01(ctx context.Context, domain Domain) (CertificateBundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CertificateBundle{}, ErrIssuerUnavailable
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{domain.Hostname}}, key)
	if err != nil {
		return CertificateBundle{}, ErrInvalid
	}
	bundle, err := i.issueDNS01CSR(ctx, domain, []string{domain.Hostname}, csr)
	if err != nil {
		return CertificateBundle{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return CertificateBundle{}, ErrCertificateInvalid
	}
	defer clear(der)
	bundle.PrivateKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return bundle, nil
}

func (i *ACMEIssuer) issueDNS01CSR(ctx context.Context, domain Domain, names []string, csr []byte) (_ CertificateBundle, returnErr error) {
	request, err := x509.ParseCertificateRequest(csr)
	if err != nil || request.CheckSignature() != nil || len(request.IPAddresses)+len(request.EmailAddresses)+len(request.URIs) != 0 {
		return CertificateBundle{}, ErrInvalid
	}
	actual, expected := slices.Clone(request.DNSNames), slices.Clone(names)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) || request.Subject.CommonName != "" && !slices.Contains(names, request.Subject.CommonName) {
		return CertificateBundle{}, ErrInvalid
	}
	for _, name := range names {
		clean, _, err := normalizeHostname(name)
		if err != nil || clean != name {
			return CertificateBundle{}, ErrInvalid
		}
	}

	ctx = certificateContext(ctx)
	if i == nil || i.dns == nil && i.http01 == nil {
		return CertificateBundle{}, ErrDNSChallengeUnavailable
	}
	// Bound the entire order, including ACME polling between fast HTTP
	// exchanges. HTTPClient.Timeout alone only bounds individual requests.
	ctx, cancelOrder := context.WithTimeout(ctx, i.timeout)
	defer cancelOrder()
	if err := acmeContextError(ctx); err != nil {
		return CertificateBundle{}, err
	}
	if err := domain.Validate(); err != nil {
		return CertificateBundle{}, err
	}
	_, wildcard, err := normalizeHostname(domain.Hostname)
	if err != nil {
		return CertificateBundle{}, err
	}
	if wildcard && domain.Strategy == StrategyOnDemandLeaf && !domain.AllowOnDemandFallback {
		return CertificateBundle{}, fmt.Errorf("%w: wildcard fallback is not enabled", ErrInvalid)
	}
	var challengeTarget string
	if i.dns != nil {
		if domain.Target().normalizedKind() == TargetPlatformWildcard {
			challengeTarget, err = PlatformDNSChallengeTargetForDomain(domain)
		} else {
			if i.challengeZone == "" {
				return CertificateBundle{}, fmt.Errorf("%w: Paperboat challenge zone is not configured", ErrDNSChallengeUnavailable)
			}
			challengeTarget, err = DelegatedChallengeTargetForDomain(domain, i.challengeZone)
		}
		if err != nil {
			return CertificateBundle{}, err
		}
	}
	slog.InfoContext(ctx, "certificate ACME phase", "phase", "order_authorization")
	client, err := i.client(ctx)
	if err != nil {
		return CertificateBundle{}, err
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(names...))
	if err != nil {
		return CertificateBundle{}, mapACMEError(err)
	}
	var withdraws []func(context.Context) error
	defer func() {
		if len(withdraws) == 0 {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), i.cleanupTimeout)
		defer cancel()
		for index := len(withdraws) - 1; index >= 0; index-- {
			if err := withdraws[index](cleanupCtx); err != nil {
				cleanupErr := fmt.Errorf("%w: challenge cleanup failed", ErrDNSChallengeUnavailable)
				returnErr = errors.Join(returnErr, cleanupErr)
			}
		}
	}()
	loggedChallenge, loggedAuthorization := false, false
	for _, authorizationURL := range order.AuthzURLs {
		authorization, err := client.GetAuthorization(ctx, authorizationURL)
		if err != nil {
			return CertificateBundle{}, mapACMEError(err)
		}
		if authorization.Status == acme.StatusValid {
			continue
		}
		if !loggedChallenge {
			slog.InfoContext(ctx, "certificate ACME phase", "phase", i.challengePhase())
			loggedChallenge = true
		}
		challenge, withdraw, err := i.presentChallenge(ctx, client, domain, names, challengeTarget, authorization)
		if withdraw != nil {
			withdraws = append(withdraws, withdraw)
		}
		if err != nil {
			return CertificateBundle{}, err
		}
		if !loggedAuthorization {
			slog.InfoContext(ctx, "certificate ACME phase", "phase", "authorization")
			loggedAuthorization = true
		}
		if _, err := client.Accept(ctx, challenge); err != nil {
			return CertificateBundle{}, mapACMEError(err)
		}
		completed, err := client.WaitAuthorization(ctx, authorization.URI)
		if err != nil {
			return CertificateBundle{}, mapACMEError(err)
		}
		if completed == nil || completed.Status != acme.StatusValid {
			return CertificateBundle{}, fmt.Errorf("%w: challenge authorization did not become valid", ErrDNSChallengePending)
		}
	}
	slog.InfoContext(ctx, "certificate ACME phase", "phase", "order")
	readyOrder, err := client.WaitOrder(ctx, order.URI)
	if err != nil {
		return CertificateBundle{}, mapACMEError(err)
	}
	if readyOrder == nil || readyOrder.FinalizeURL == "" {
		return CertificateBundle{}, ErrIssuerUnavailable
	}
	slog.InfoContext(ctx, "certificate ACME phase", "phase", "finalize")
	der, _, err := client.CreateOrderCert(ctx, readyOrder.FinalizeURL, csr, true)
	if err != nil {
		// Some RFC 8555 test authorities return a valid order from finalize
		// before populating its certificate URL in that response. Re-fetch the
		// durable order and certificate only when it is already valid; this is
		// bounded and preserves the original error for every other failure.
		latest, refreshErr := client.GetOrder(ctx, order.URI)
		if refreshErr == nil && latest != nil && latest.Status == acme.StatusValid && latest.CertURL != "" {
			der, refreshErr = client.FetchCert(ctx, latest.CertURL, true)
			err = refreshErr
		}
	}
	if err != nil {
		return CertificateBundle{}, mapACMEError(err)
	}
	if len(der) == 0 || len(der) > maxACMEChainCertificates {
		return CertificateBundle{}, fmt.Errorf("%w: ACME returned an invalid chain", ErrCertificateInvalid)
	}
	var leaf *x509.Certificate
	var certificatePEM []byte
	for index, raw := range der {
		if len(raw) == 0 || len(raw) > maxACMECertificateDERBytes {
			return CertificateBundle{}, fmt.Errorf("%w: ACME certificate is oversized", ErrCertificateInvalid)
		}
		parsed, err := x509.ParseCertificate(raw)
		if err != nil {
			return CertificateBundle{}, fmt.Errorf("%w: ACME certificate is invalid", ErrCertificateInvalid)
		}
		if index == 0 {
			leaf = parsed
		}
		certificatePEM = append(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})...)
	}
	if leaf == nil {
		return CertificateBundle{}, ErrCertificateInvalid
	}
	actual = slices.Clone(leaf.DNSNames)
	slices.Sort(actual)
	if !slices.Equal(actual, expected) || !bytes.Equal(leaf.RawSubjectPublicKeyInfo, request.RawSubjectPublicKeyInfo) || leaf.IsCA || !leaf.NotAfter.After(time.Now()) {
		return CertificateBundle{}, ErrCertificateInvalid
	}
	return CertificateBundle{CertificatePEM: certificatePEM, Issuer: i.issuer, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter}, returnErr
}

func acmeContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (i *ACMEIssuer) client(ctx context.Context) (*acme.Client, error) {
	ctx = certificateContext(ctx)
	i.accountMu.Lock()
	defer i.accountMu.Unlock()
	key := i.accountKey
	if key == nil {
		var err error
		key, err = i.accountKeys.ResolveSigner(ctx, i.accountKeyRef)
		if err != nil || key == nil || !supportedAccountKey(key) {
			return nil, fmt.Errorf("%w: ACME account key resolution failed", ErrMasterKeyUnavailable)
		}
	}
	client := &acme.Client{Key: key, KID: i.accountKID, DirectoryURL: i.directoryURL, HTTPClient: i.httpClient, UserAgent: "paperboat-tunnelcert/1"}
	client.RetryBackoff = func(attempt int, request *http.Request, response *http.Response) time.Duration {
		if attempt >= i.maxAttempts {
			return -1
		}
		if response != nil {
			if retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC()); retryAfter > 0 {
				if retryAfter > i.retryMax {
					return i.retryMax
				}
				return retryAfter
			}
		}
		backoff := i.retryBase
		for step := 1; step < attempt && backoff < i.retryMax/2; step++ {
			backoff *= 2
		}
		if backoff > i.retryMax {
			backoff = i.retryMax
		}
		return backoff
	}
	if client.KID == "" {
		contact := []string(nil)
		if i.accountEmail != "" {
			contact = []string{"mailto:" + i.accountEmail}
		}
		if _, err := client.Register(ctx, &acme.Account{Contact: contact}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
			return nil, mapACMEError(err)
		}
		// Register sets KID even when the CA reports that this key already
		// exists. Retain that public account URL in memory for renewal and
		// revocation without persisting or returning key material.
		i.accountKID = client.KID
	}
	return client, nil
}

func findChallenge(authorization *acme.Authorization, challengeType string) *acme.Challenge {
	if authorization == nil {
		return nil
	}
	for _, challenge := range authorization.Challenges {
		if challenge != nil && challenge.Type == challengeType && challenge.Status != acme.StatusInvalid {
			return challenge
		}
	}
	return nil
}

func (i *ACMEIssuer) challengePhase() string {
	if i.http01 != nil {
		return "http_publish"
	}
	return "dns_propagation"
}

// presentChallenge answers one pending authorization with the configured
// challenge type. withdraw is non-nil as soon as the response is visible to
// the challenge's verifier, so the caller removes it even when a later step
// fails.
func (i *ACMEIssuer) presentChallenge(ctx context.Context, client *acme.Client, domain Domain, names []string, challengeTarget string, authorization *acme.Authorization) (*acme.Challenge, func(context.Context) error, error) {
	if i.http01 != nil {
		return i.presentHTTP01(ctx, client, domain, names, authorization)
	}
	return i.presentDNS01(ctx, client, names, challengeTarget, domain, authorization)
}

func (i *ACMEIssuer) presentDNS01(ctx context.Context, client *acme.Client, names []string, challengeTarget string, domain Domain, authorization *acme.Authorization) (*acme.Challenge, func(context.Context) error, error) {
	challenge := findChallenge(authorization, "dns-01")
	if challenge == nil || challenge.Token == "" {
		return nil, nil, ErrDNSChallengeUnavailable
	}
	value, err := client.DNS01ChallengeRecord(challenge.Token)
	if err != nil || len(value) == 0 || len(value) > maxACMEDNSRecordValueLength {
		return nil, nil, ErrDNSChallengeUnavailable
	}
	authorizationName, err := authorizationIdentifier(authorization, names)
	if err != nil {
		return nil, nil, err
	}
	challengeHost := "_acme-challenge." + strings.TrimPrefix(authorizationName, "*.")
	recordTarget := challengeTarget
	if domain.TargetKind == TargetPlatformWildcard {
		recordTarget = challengeHost
	}
	record := DNS01Record{Domain: authorizationName, Name: recordTarget, TargetName: recordTarget, AuthorizationName: challengeHost, Value: value, TTL: 60 * time.Second}
	record, err = i.dns.Present(ctx, record)
	if err != nil {
		return nil, nil, mapDNSProviderError(err)
	}
	withdraw := func(ctx context.Context) error { return i.dns.Cleanup(ctx, record) }
	if record.Name != recordTarget || record.TargetName != recordTarget || record.AuthorizationName != challengeHost || record.Value != value || record.ProviderID == "" {
		return nil, withdraw, ErrDNSChallengeUnavailable
	}
	waitCtx, cancel := context.WithTimeout(ctx, i.propagationTimeout)
	err = i.dns.Wait(waitCtx, record)
	cancel()
	if err != nil {
		return nil, withdraw, mapDNSProviderError(err)
	}
	return challenge, withdraw, nil
}

// presentHTTP01 publishes the key authorization to every edge that serves the
// domain and waits until each has fetched it. Pebble-style verifiers connect
// to the edge's public port 80, so the edge must have the response first.
func (i *ACMEIssuer) presentHTTP01(ctx context.Context, client *acme.Client, domain Domain, names []string, authorization *acme.Authorization) (*acme.Challenge, func(context.Context) error, error) {
	challenge := findChallenge(authorization, "http-01")
	if challenge == nil || challenge.Token == "" {
		return nil, nil, ErrDNSChallengeUnavailable
	}
	keyAuthorization, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return nil, nil, ErrDNSChallengeUnavailable
	}
	authorizationName, err := authorizationIdentifier(authorization, names)
	if err != nil {
		return nil, nil, err
	}
	edges := make([]string, 0, len(domain.EdgeTargets))
	for _, target := range domain.EdgeTargets {
		edges = append(edges, target.NodeID)
	}
	if len(edges) == 0 {
		return nil, nil, fmt.Errorf("%w: no edge serves the domain for HTTP-01", ErrDNSChallengeUnavailable)
	}
	record := HTTP01Record{Hostname: authorizationName, Token: challenge.Token, KeyAuthorization: keyAuthorization, EdgeNodeIDs: edges}
	if err := i.http01.Publish(ctx, record); err != nil {
		return nil, nil, mapDNSProviderError(err)
	}
	withdraw := func(ctx context.Context) error { return i.http01.Withdraw(ctx, record) }
	waitCtx, cancel := context.WithTimeout(ctx, i.propagationTimeout)
	err = i.http01.Wait(waitCtx, record)
	cancel()
	if err != nil {
		return nil, withdraw, mapDNSProviderError(err)
	}
	return challenge, withdraw, nil
}

// authorizationIdentifier returns the authorized name, which must be one of
// the names the order requested (a wildcard authorization covers "*."+name).
func authorizationIdentifier(authorization *acme.Authorization, names []string) (string, error) {
	authorizationName := authorization.Identifier.Value
	if !slices.Contains(names, authorizationName) && !slices.Contains(names, "*."+authorizationName) {
		return "", ErrInvalid
	}
	return authorizationName, nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func mapDNSProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(ErrDNSChallengeUnavailable, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrDNSChallengeUnavailable, context.DeadlineExceeded)
	}
	if errors.Is(err, ErrDNSChallengePending) || errors.Is(err, ErrDNSChallengeUnavailable) {
		return err
	}
	return fmt.Errorf("%w: DNS provider operation failed", ErrDNSChallengeUnavailable)
}

func mapACMEError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(ErrIssuerUnavailable, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrIssuerUnavailable, context.DeadlineExceeded)
	}
	var acmeErr *acme.Error
	if errors.As(err, &acmeErr) && acmeErr != nil {
		if acmeErr.StatusCode == http.StatusTooManyRequests {
			return &RateLimitError{RetryAt: acmeRetryTime(acmeErr.Header.Get("Retry-After"), time.Now()), status: acmeErr.StatusCode, cause: err}
		}
		if acmeErr.StatusCode >= 500 || acmeErr.StatusCode == http.StatusRequestTimeout || acmeErr.StatusCode == http.StatusTooEarly {
			return acmeFailure{kind: acmeFailureUpstream, status: acmeErr.StatusCode, sentinel: ErrIssuerUnavailable, cause: err}
		}
		if acmeDNSProblem(acmeErr.ProblemType) {
			return acmeFailure{kind: acmeFailureDNS, status: acmeErr.StatusCode, sentinel: ErrDNSChallengeUnavailable, cause: err}
		}
		return acmeFailure{kind: acmeFailureRejected, status: acmeErr.StatusCode, sentinel: ErrIssuerUnavailable, cause: err}
	}
	var authorizationErr *acme.AuthorizationError
	if errors.As(err, &authorizationErr) && authorizationErr != nil {
		return acmeFailure{kind: acmeFailureAuthorization, sentinel: ErrDNSChallengeUnavailable, cause: err}
	}
	var orderErr *acme.OrderError
	if errors.As(err, &orderErr) && orderErr != nil && orderErr.Problem != nil {
		return mapACMEError(orderErr.Problem)
	}
	return acmeFailure{kind: acmeFailureTransport, sentinel: ErrIssuerUnavailable, cause: err}
}

func acmeDNSProblem(problemType string) bool {
	switch strings.ToLower(strings.TrimSpace(problemType)) {
	case "dns", "urn:ietf:params:acme:error:dns":
		return true
	default:
		return false
	}
}

type acmeFailureKind uint8

const (
	acmeFailureTransport acmeFailureKind = iota + 1
	acmeFailureUpstream
	acmeFailureDNS
	acmeFailureRejected
	acmeFailureAuthorization
)

type acmeFailure struct {
	kind     acmeFailureKind
	status   int
	sentinel error
	cause    error
}

// IsExpectedRetryableProviderOutcome reports only finite ACME responses that
// are safe to keep as durable retry/status outcomes rather than unexpected
// worker exceptions. It recognizes the exact local mapper constructors through
// ordinary unary wrappers; joined or unknown errors remain operational.
func IsExpectedRetryableProviderOutcome(err error) (expected bool) {
	defer func() {
		if recover() != nil {
			expected = false
		}
	}()
	for depth := 0; err != nil && depth < 32; depth++ {
		switch value := err.(type) {
		case acmeFailure:
			return value.expectedProviderResponse()
		case *RateLimitError:
			return value != nil && value.status == http.StatusTooManyRequests && matchingACMEStatus(value.cause, value.status)
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

func (e acmeFailure) expectedProviderResponse() bool {
	if e.kind == acmeFailureAuthorization {
		_, known := e.cause.(*acme.AuthorizationError)
		return known && e.sentinel == ErrDNSChallengeUnavailable
	}
	if e.status < http.StatusBadRequest || e.status >= http.StatusInternalServerError || e.status == http.StatusTooManyRequests || !matchingACMEStatus(e.cause, e.status) {
		return false
	}
	switch e.kind {
	case acmeFailureDNS:
		return e.sentinel == ErrDNSChallengeUnavailable
	case acmeFailureRejected:
		return e.sentinel == ErrIssuerUnavailable
	default:
		return false
	}
}

func matchingACMEStatus(err error, status int) bool {
	providerError, ok := err.(*acme.Error)
	return ok && providerError != nil && providerError.StatusCode == status
}

func (e acmeFailure) Error() string {
	switch e.kind {
	case acmeFailureUpstream:
		return "certificate issuer returned an upstream failure"
	case acmeFailureDNS, acmeFailureAuthorization:
		return "certificate DNS challenge failed"
	case acmeFailureRejected:
		return "certificate issuer rejected the request"
	default:
		return "certificate issuer request failed"
	}
}

func (e acmeFailure) Unwrap() []error {
	if e.sentinel == nil {
		return []error{e.cause}
	}
	return []error{e.sentinel, e.cause}
}

func (e acmeFailure) DiagnosticStatus() int { return e.status }

func supportedAccountKey(key crypto.Signer) bool {
	switch key.(type) {
	case *ecdsa.PrivateKey:
		return true
	case interface{ Public() crypto.PublicKey }:
		_, rsaOK := key.Public().(*rsa.PublicKey)
		return rsaOK
	default:
		return false
	}
}

// RateLimitError retains only the retry boundary, never ACME problem details.
type RateLimitError struct {
	RetryAt time.Time
	status  int
	cause   error
}

func (e *RateLimitError) Error() string         { return "certificate issuer rate limited" }
func (e *RateLimitError) Unwrap() []error       { return []error{ErrIssuerRateLimited, e.cause} }
func (e *RateLimitError) DiagnosticStatus() int { return e.status }
func acmeRetryTime(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds < 365*24*60*60 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) && at.Before(now.Add(365*24*time.Hour)) {
		return at
	}
	return now.Add(7 * 24 * time.Hour)
}
