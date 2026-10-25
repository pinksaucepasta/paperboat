package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
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
