package app

import (
	"context"
	"crypto/subtle"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorinfra"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
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
	runtime, err := tunnelv1.NewCertificateRuntime(tunnelv1.CertificateRuntimeConfig{
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
	return runtime, nil
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
	signerSource := tunnelcert.EnvironmentSignerSource{EnvironmentReferenceSource: secrets}
	issuerConfig := tunnelcert.ACMEIssuerConfig{
		DirectoryURL:        certificateConfig.DirectoryURL,
		AccountKID:          certificateConfig.AccountKID,
		AccountEmail:        certificateConfig.AccountEmail,
		AccountKeys:         signerSource,
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
		ZoneID: certificateConfig.PublicDNSZoneID, TokenReference: certificateConfig.DNSTokenReference,
		TokenSource: secrets, HTTPClient: providerHTTPClient("cloudflare-public-dns", cfg.HTTP.RequestTimeout),
		WriteBase: certificateConfig.DNSWriteBase,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: public DNS provider configuration: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	dnsZones, err := tunnelcert.NewDNS01Zones(map[string]tunnelcert.DNS01Provider{
		cfg.Preview.BaseDomain:          publicDNS,
		cfg.RuntimeBaseDomain:           dnsProvider,
		certificateConfig.ChallengeZone: dnsProvider,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: configured DNS zones: %w", tunnelv1.ErrCertificateRuntimeUnavailable, err)
	}
	return dnsZones, nil
}
