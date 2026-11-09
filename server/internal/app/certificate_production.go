package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorinfra"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workspacedomain"
)

// newCertificateRuntime is the server's concrete managed-TLS composition.
// Config supplies only public endpoints and opaque references; the environment
// adapter is a narrow deployment boundary for an injected secret manager.
// Production deployments can replace that adapter without changing the certificate
// worker, durable store, or edge distribution contract.
func newCertificateRuntime(ctx context.Context, database *db.DB, cfg config.Config) (*tunnelv1.CertificateRuntime, error) {
	certificateConfig := cfg.Certificates
	if !certificateConfig.Enabled {
		return nil, fmt.Errorf("%w: managed certificates are disabled", tunnelv1.ErrCertificateRuntimeUnavailable)
	}
	if err := certificateConfig.Validate(cfg.Environment); err != nil {
		return nil, fmt.Errorf("%w: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	if database == nil {
		return nil, fmt.Errorf("%w: database is required", tunnelv1.ErrCertificateRuntimeUnavailable)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	secrets := tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: os.LookupEnv}
	distributionCredential, err := secrets.Resolve(ctx, certificateConfig.DistributionCredentialReference)
	if err != nil {
		return nil, fmt.Errorf("%w: distribution credential reference is unavailable: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	if len(distributionCredential) == 0 {
		return nil, fmt.Errorf("%w: distribution credential reference is unavailable", tunnelv1.ErrCertificateRuntimeUnavailable)
	}
	defer clear(distributionCredential)
	// The edge distribution client currently authenticates with the same
	// server-issued control credential. Keep the two configuration references
	// explicit, but fail closed if they resolve to different bytes. A second
	// independent bearer would let a misconfigured edge pull a different
	// node's certificate queue.
	edgeCredential := []byte(cfg.Secrets.EdgeControlCredential)
	if subtle.ConstantTimeCompare(edgeCredential, distributionCredential) != 1 {
		clear(edgeCredential)
		return nil, fmt.Errorf("%w: distribution credential must equal edge control credential", tunnelv1.ErrCertificateRuntimeUnavailable)
	}
	clear(edgeCredential)
	issuer, caaInspector, http01, err := newPublicCertificateIssuer(cfg)
	if err != nil {
		return nil, err
	}
	platformEdges, err := tunnelv1.NewSQLPlatformEdgeTargetResolver(database, controlplane.ControlTunnelNodeStaleAfter(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: platform edge targets: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	platformBases, platformTargets := platformWildcardInputs(certificateConfig.ChallengeType(), tunnelcert.PlatformCertificateBases{
		PreviewBaseDomain: cfg.Preview.BaseDomain,
		TunnelBaseDomain:  cfg.Tunnel.BaseDomain,
		RuntimeBaseDomain: cfg.RuntimeBaseDomain,
	}, platformEdges.Resolve)
	// Wildcard certificates need DNS-01: an http-01 deployment checks workspace domains but issues nothing.
	var namesIssuer workspacedomain.NamesIssuer
	if certificateConfig.ChallengeType() == config.CertificateChallengeDNS01 {
		namesIssuer = issuer
	}
	domains, err := workspacedomain.NewWorker(database, workspaceDomainConfig(cfg), workspacedomain.WorkerConfig{
		Issuer: namesIssuer, CAA: caaInspector, Keys: secrets, MasterKeyReference: certificateConfig.MasterKeyReference,
		IssuerName: certificateConfig.Issuer, OwnerID: certificateConfig.OwnerID, RenewBefore: certificateConfig.RenewBefore,
		LockTTL: certificateConfig.LockTTL, MaxLifetime: certificateConfig.MaxCertificateLifetime,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: workspace domains: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	runtime, err := tunnelv1.NewCertificateRuntime(tunnelv1.CertificateRuntimeConfig{
		WorkspaceCertificates:  domains,
		NodeCredentials:        nodeauth.Composite{operatorinfra.NewService(database, cfg.Secrets.EncryptionKey), selfhost.New(database, cfg.Secrets.EncryptionKey)},
		Database:               database,
		Issuer:                 issuer,
		CAA:                    caaInspector,
		Keys:                   secrets,
		MasterKeyReference:     certificateConfig.MasterKeyReference,
		DistributionCredential: distributionCredential,
		IssuerName:             certificateConfig.Issuer,
		OwnerID:                certificateConfig.OwnerID,
		RenewBefore:            certificateConfig.RenewBefore,
		LockTTL:                certificateConfig.LockTTL,
		DistributionTimeout:    certificateConfig.DistributionTimeout,
		ExpiryAlertWindow:      certificateConfig.ExpiryAlertWindow,
		MaxCertificateLifetime: certificateConfig.MaxCertificateLifetime,
		PlatformBases:          platformBases,
		PlatformEdgeTargets:    platformTargets,
	})
	if err != nil {
		return nil, err
	}
	runtime.HTTP01 = http01
	runtime.WorkspaceDomains = domains
	return runtime, nil
}

func workspaceDomainConfig(cfg config.Config) workspacedomain.Config {
	return workspacedomain.Config{ChallengeZone: cfg.Certificates.ChallengeZone, RoutingBase: cfg.Tunnel.BaseDomain}
}

// platformWildcardInputs returns the server-owned wildcard inputs. Wildcard
// certificates need DNS-01, so under http-01 the platform worker is not started
// (the runtime starts it only when bases or an edge resolver are given).
func platformWildcardInputs(challenge string, bases tunnelcert.PlatformCertificateBases, resolve tunnelv1.PlatformEdgeTargetResolver) (tunnelcert.PlatformCertificateBases, tunnelv1.PlatformEdgeTargetResolver) {
	if challenge == config.CertificateChallengeHTTP01 {
		return tunnelcert.PlatformCertificateBases{}, nil
	}
	return bases, resolve
}

// newPublicCertificateIssuer configures the ACME issuer for managed tunnel
// certificates. dns-01 (the default) needs the Cloudflare zones; http-01 (staging
// only) answers through the edges and returns the responses the edge snapshot serves.
func newPublicCertificateIssuer(cfg config.Config) (*tunnelcert.ACMEIssuer, tunnelcert.CAAInspector, *tunnelcert.HTTP01Responses, error) {
	certificateConfig := cfg.Certificates
	secrets := tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: os.LookupEnv}
	issuerConfig := acmeIssuerConfig(cfg, secrets)
	var http01 *tunnelcert.HTTP01Responses
	if certificateConfig.ChallengeType() == config.CertificateChallengeHTTP01 {
		http01 = tunnelcert.NewHTTP01Responses(func() time.Time { return time.Now().UTC() })
		issuerConfig.HTTP01 = http01
	} else {
		dnsZones, err := newCloudflareDNSZones(cfg, secrets)
		if err != nil {
			return nil, nil, nil, err
		}
		issuerConfig.DNS = dnsZones
	}
	caaInspector, err := tunnelcert.NewDNSCAAInspector(tunnelcert.DNSCAAInspectorConfig{Server: certificateConfig.CAAResolver, Timeout: cfg.HTTP.RequestTimeout})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: CAA resolver configuration: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	issuer, err := tunnelcert.NewACMEIssuer(issuerConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: ACME issuer configuration: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	return issuer, caaInspector, http01, nil
}

// newCloudflareDNSZones routes DNS-01 records to the Cloudflare zones named in configuration.
func newCloudflareDNSZones(cfg config.Config, secrets tunnelcert.EnvironmentReferenceSource) (*tunnelcert.DNS01Zones, error) {
	certificateConfig := cfg.Certificates
	dnsProvider, err := tunnelcert.NewCloudflareDNSProvider(tunnelcert.CloudflareDNSConfig{
		Resolver:       zoneResolver,
		ZoneID:         certificateConfig.DNSZoneID,
		TokenReference: certificateConfig.DNSTokenReference,
		TokenSource:    secrets,
		HTTPClient:     providerHTTPClient("cloudflare-dns", cfg.HTTP.RequestTimeout),
		WriteBase:      certificateConfig.DNSWriteBase,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: DNS provider configuration: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	publicDNS, err := tunnelcert.NewCloudflareDNSProvider(tunnelcert.CloudflareDNSConfig{
		Resolver: zoneResolver, ZoneID: certificateConfig.PublicDNSZoneID, TokenReference: certificateConfig.DNSTokenReference,
		TokenSource: secrets, HTTPClient: providerHTTPClient("cloudflare-public-dns", cfg.HTTP.RequestTimeout),
		WriteBase: certificateConfig.DNSWriteBase,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: public DNS provider configuration: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	zones := map[string]tunnelcert.DNS01Provider{
		cfg.Preview.BaseDomain:          publicDNS,
		cfg.RuntimeBaseDomain:           dnsProvider,
		certificateConfig.ChallengeZone: dnsProvider,
	}
	dnsZones, err := tunnelcert.NewDNS01Zones(zones)
	if err != nil {
		return nil, fmt.Errorf("%w: configured DNS zones: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	return dnsZones, nil
}

// acmeIssuerConfig is the ACME account and timing every issuer shares; callers add the challenge path.
func acmeIssuerConfig(cfg config.Config, secrets tunnelcert.EnvironmentReferenceSource) tunnelcert.ACMEIssuerConfig {
	certificateConfig := cfg.Certificates
	return tunnelcert.ACMEIssuerConfig{
		DirectoryURL:        certificateConfig.DirectoryURL,
		AccountKID:          certificateConfig.AccountKID,
		AccountEmail:        certificateConfig.AccountEmail,
		AccountKeys:         tunnelcert.EnvironmentSignerSource{EnvironmentReferenceSource: secrets},
		AccountKeyReference: certificateConfig.AccountKeyReference,
		HTTPClient:          providerHTTPClient("acme", cfg.HTTP.RequestTimeout),
		Timeout:             certificateConfig.ACMETimeout,
		PropagationTimeout:  certificateConfig.PropagationTimeout,
		CleanupTimeout:      certificateConfig.CleanupTimeout,
		PollInterval:        certificateConfig.PollInterval,
		MaxAttempts:         certificateConfig.MaxAttempts,
		RetryBase:           certificateConfig.RetryBase,
		RetryMax:            certificateConfig.RetryMax,
		Issuer:              certificateConfig.Issuer,
		ChallengeZone:       certificateConfig.ChallengeZone,
	}
}

// newRegionalCertificateIssuer issues the server-node wildcard *.<regional_domain> with DNS-01 in the regional
// zone only, so a node without edges (no preview or runtime domain) can issue it too.
func newRegionalCertificateIssuer(cfg config.Config) (*tunnelcert.ACMEIssuer, error) {
	secrets := tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: os.LookupEnv}
	if err := checkRegionalCertificateSecrets(cfg, secrets); err != nil {
		return nil, err
	}
	provider, err := tunnelcert.NewCloudflareDNSProvider(tunnelcert.CloudflareDNSConfig{
		ZoneID: cfg.RegionalDNSZoneID, TokenReference: cfg.Certificates.DNSTokenReference, TokenSource: secrets,
		HTTPClient: providerHTTPClient("cloudflare-regional-dns", cfg.HTTP.RequestTimeout), WriteBase: cfg.Certificates.DNSWriteBase,
		Resolver: zoneResolver,
	})
	if err != nil {
		return nil, fmt.Errorf("regional DNS provider: %w", err)
	}
	zones, err := tunnelcert.NewDNS01Zones(map[string]tunnelcert.DNS01Provider{cfg.RegionalDomain: provider})
	if err != nil {
		return nil, fmt.Errorf("regional DNS zone: %w", err)
	}
	issuerConfig := acmeIssuerConfig(cfg, secrets)
	issuerConfig.DNS = zones
	return tunnelcert.NewACMEIssuer(issuerConfig)
}

// checkRegionalCertificateSecrets refuses to start a regional node whose DNS token or ACME account key is not
// configured or does not resolve, naming the setting, instead of failing every issuance later.
func checkRegionalCertificateSecrets(cfg config.Config, secrets tunnelcert.EnvironmentReferenceSource) error {
	c := cfg.Certificates
	for _, setting := range []struct{ name, reference string }{
		{"certificates.dns_token_reference (PAPERBOAT_CERTIFICATES_DNS_TOKEN_REFERENCE)", c.DNSTokenReference},
		{"certificates.account_key_reference (PAPERBOAT_CERTIFICATES_ACCOUNT_KEY_REFERENCE)", c.AccountKeyReference},
		{"certificates.directory_url (PAPERBOAT_CERTIFICATES_DIRECTORY_URL)", c.DirectoryURL},
	} {
		if strings.TrimSpace(setting.reference) == "" {
			return fmt.Errorf("regional_domain is set but %s is not: the regional certificate cannot be issued", setting.name)
		}
	}
	ctx := context.Background()
	if token, err := secrets.Resolve(ctx, c.DNSTokenReference); err != nil || len(token) == 0 {
		name, _ := tunnelcert.EnvironmentReferenceName(secrets.Prefix, c.DNSTokenReference)
		return fmt.Errorf("regional_domain is set but the DNS token %s does not resolve: set %s", c.DNSTokenReference, name)
	}
	if _, err := (tunnelcert.EnvironmentSignerSource{EnvironmentReferenceSource: secrets}).ResolveSigner(ctx, c.AccountKeyReference); err != nil {
		name, _ := tunnelcert.EnvironmentReferenceName(secrets.Prefix, c.AccountKeyReference)
		return fmt.Errorf("regional_domain is set but the ACME account key %s does not resolve to a usable private key: set %s", c.AccountKeyReference, name)
	}
	return nil
}

// authoritativeResolver checks DNS-01 and address propagation at the zone's own nameservers, not a recursive resolver: a
// recursive resolver caches the first NXDOMAIN for the zone's SOA minimum (up to 30 min), so every attempt in that
// window looked pending. Prior art: lego checks the authoritative nameservers (challenge/dns01/precheck.go).
type authoritativeResolver struct {
	lookupNS func(context.Context, string) ([]*net.NS, error)
}

// zoneResolver is the one resolver every DNS-01 and address propagation check uses.
var zoneResolver = authoritativeResolver{lookupNS: net.DefaultResolver.LookupNS}

// nameservers walks up from name to the closest enclosing zone that has NS records.
func (r authoritativeResolver) nameservers(ctx context.Context, name string) ([]string, error) {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		ns, err := r.lookupNS(ctx, strings.Join(labels[i:], "."))
		if err != nil || len(ns) == 0 {
			continue
		}
		out := make([]string, 0, len(ns))
		for _, n := range ns {
			out = append(out, strings.TrimSuffix(n.Host, "."))
		}
		return out, nil
	}
	return nil, fmt.Errorf("no nameservers found for %s", name)
}

func (r authoritativeResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return r.ask(ctx, name, (*net.Resolver).LookupTXT)
}

func (r authoritativeResolver) LookupHost(ctx context.Context, name string) ([]string, error) {
	return r.ask(ctx, name, (*net.Resolver).LookupHost)
}

// ask sends the query to each authoritative nameserver until one answers; a not-found answer is an answer.
func (r authoritativeResolver) ask(ctx context.Context, name string, query func(*net.Resolver, context.Context, string) ([]string, error)) ([]string, error) {
	servers, err := r.nameservers(ctx, name)
	if err != nil {
		return nil, err
	}
	var last error
	for _, server := range servers {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(server, "53"))
		}}
		values, err := query(resolver, ctx, name)
		var dnsErr *net.DNSError
		if err == nil || errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return values, err
		}
		last = err
	}
	return nil, last
}
