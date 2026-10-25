package config

import (
	"context"
	"strings"
	"testing"
)

// TestCertificateChallengeHTTP01IsRefusedInProduction mirrors the
// selfhost_allow_private_ingress guard: http-01 is a staging-only ACME path.
func TestCertificateChallengeHTTP01IsRefusedInProduction(t *testing.T) {
	for _, env := range []Environment{EnvironmentProduction, EnvironmentDevelopment} {
		c := Default()
		c.Environment = env
		c.Certificates = Certificates{
			Enabled:                         true,
			DirectoryURL:                    "https://pebble.example.test:14000/dir",
			Issuer:                          "pebble",
			Challenge:                       CertificateChallengeHTTP01,
			AccountKeyReference:             "secret://acme/account",
			MasterKeyReference:              "secret://paperboat/master",
			DistributionCredentialReference: "secret://edge/distribution",
			OwnerID:                         "server_01",
			CAAResolver:                     "127.0.0.1:53",
		}
		err := c.Validate()
		refused := err != nil && strings.Contains(err.Error(), "PAPERBOAT_CERTIFICATES_CHALLENGE")
		if refused != (env == EnvironmentProduction) {
			t.Fatalf("%s: http-01 refused=%v (%v)", env, refused, err)
		}
		if err != nil && strings.Contains(err.Error(), "certificates.dns_") {
			t.Fatalf("%s: http-01 still requires DNS-01 inputs: %v", env, err)
		}
	}
}

func TestCertificateChallengeRejectsUnknownType(t *testing.T) {
	c := Default()
	c.Certificates.Enabled = true
	c.Certificates.Challenge = "tls-alpn-01"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "certificates.challenge must be") {
		t.Fatalf("unknown challenge type accepted: %v", err)
	}
}

func TestCertificateChallengeEnvironmentSelectsHTTP01(t *testing.T) {
	cfg, err := Load(context.Background(), LoadOptions{
		LookupEnv: func(name string) (string, bool) {
			values := map[string]string{"PAPERBOAT_CERTIFICATES_CHALLENGE": "http-01"}
			value, ok := values[name]
			return value, ok
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Certificates.Challenge != CertificateChallengeHTTP01 {
		t.Fatalf("challenge = %q", cfg.Certificates.Challenge)
	}
	if cfg2, err := Load(context.Background(), LoadOptions{}); err != nil || cfg2.Certificates.Challenge != CertificateChallengeDNS01 {
		t.Fatalf("default challenge = %q (%v)", cfg2.Certificates.Challenge, err)
	}
}
