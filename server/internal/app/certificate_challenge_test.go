package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

// http-01 composes without any Cloudflare zone, which is what a staging stack
// has. dns-01 with the same missing zones must still refuse to start.
func TestCertificateChallengeComposition(t *testing.T) {
	cfg := config.Default()
	cfg.Certificates = config.Certificates{
		Enabled:                         true,
		DirectoryURL:                    "https://pebble.staging.example.test:14000/dir",
		Issuer:                          "pebble",
		Challenge:                       config.CertificateChallengeHTTP01,
		AccountKeyReference:             "PAPERBOAT_CERT_SECRET_ACCOUNT",
		MasterKeyReference:              "PAPERBOAT_CERT_SECRET_MASTER",
		DistributionCredentialReference: "PAPERBOAT_CERT_SECRET_DIST",
		OwnerID:                         "server_01",
		CAAResolver:                     "127.0.0.1:53",
	}
	issuer, _, http01, err := newPublicCertificateIssuer(cfg)
	if err != nil {
		t.Fatalf("http-01 composition needs no DNS zone: %v", err)
	}
	if issuer == nil || http01 == nil {
		t.Fatal("http-01 composition did not return its issuer and response registry")
	}

	cfg.Certificates.Challenge = config.CertificateChallengeDNS01
	if _, _, _, err := newPublicCertificateIssuer(cfg); !errors.Is(err, tunnelv1.ErrCertificateRuntimeUnavailable) || !strings.Contains(err.Error(), "DNS") {
		t.Fatalf("dns-01 without Cloudflare zones accepted: %v", err)
	}
}

// Under http-01 the platform wildcard worker has no inputs, so the runtime does
// not start it; dns-01 keeps the server-owned wildcard bases.
func TestPlatformWildcardsNeedDNS01(t *testing.T) {
	bases := tunnelcert.PlatformCertificateBases{PreviewBaseDomain: "preview.example.test", TunnelBaseDomain: "preview.example.test", RuntimeBaseDomain: "runtime.example.test"}
	var resolver tunnelv1.PlatformEdgeTargetResolver = func(context.Context) ([]tunnelcert.EdgeTarget, error) { return nil, nil }
	gotBases, gotResolver := platformWildcardInputs(config.CertificateChallengeHTTP01, bases, resolver)
	if gotBases != (tunnelcert.PlatformCertificateBases{}) || gotResolver != nil {
		t.Fatalf("http-01 kept platform wildcard inputs: %+v", gotBases)
	}
	gotBases, gotResolver = platformWildcardInputs(config.CertificateChallengeDNS01, bases, resolver)
	if gotBases != bases || gotResolver == nil {
		t.Fatalf("dns-01 dropped platform wildcard inputs: %+v", gotBases)
	}
}
