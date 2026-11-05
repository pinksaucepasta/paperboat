package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// A regional node with a missing or unresolvable DNS token or ACME account key refuses to start and names the
// setting or environment variable to fix, instead of stalling every issuance.
func TestRegionalNodeRefusesToStartWithoutCertificateSecrets(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	tokenEnv, _ := tunnelcert.EnvironmentReferenceName("PAPERBOAT_CERT_SECRET_", "secret://cloudflare/dns")
	keyEnv, _ := tunnelcert.EnvironmentReferenceName("PAPERBOAT_CERT_SECRET_", "secret://acme/account")
	base := func() config.Config {
		cfg := config.Default()
		cfg.RegionalDomain = "regions.example.test"
		cfg.Certificates.DNSTokenReference = "secret://cloudflare/dns"
		cfg.Certificates.AccountKeyReference = "secret://acme/account"
		cfg.Certificates.DirectoryURL = "https://acme.example.test/directory"
		return cfg
	}
	env := func(values map[string]string) tunnelcert.EnvironmentReferenceSource {
		return tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: func(k string) (string, bool) { v, ok := values[k]; return v, ok }}
	}
	good := map[string]string{tokenEnv: "cloudflare-token-for-tests", keyEnv: keyPEM}
	if err := checkRegionalCertificateSecrets(base(), env(good)); err != nil {
		t.Fatalf("complete settings refused: %v", err)
	}
	noToken := base()
	noToken.Certificates.DNSTokenReference = ""
	for name, tc := range map[string]struct {
		cfg  config.Config
		env  map[string]string
		want string
	}{
		"no token reference": {noToken, good, "PAPERBOAT_CERTIFICATES_DNS_TOKEN_REFERENCE"},
		"token missing":      {base(), map[string]string{keyEnv: keyPEM}, tokenEnv},
		"key missing":        {base(), map[string]string{tokenEnv: "t"}, keyEnv},
		"key not a key":      {base(), map[string]string{tokenEnv: "t", keyEnv: "-----BEGIN not a key"}, keyEnv},
	} {
		err := checkRegionalCertificateSecrets(tc.cfg, env(tc.env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want an error naming %s, got %v", name, tc.want, err)
		}
	}
}
