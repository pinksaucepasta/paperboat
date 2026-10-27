package app

import (
	"context"
	"fmt"
	"os"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testfixtures"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// newTestFixtures builds the final re-check runner's scoped surface. Its DNS records are written with the server's own
// Cloudflare DNS token (the same reference the certificate coordinator uses), confined to the configured test domain.
func newTestFixtures(cfg config.Config, store *db.DB, auditWriter *audit.Writer) (*testfixtures.Service, error) {
	secrets := tunnelcert.EnvironmentReferenceSource{Prefix: "PAPERBOAT_CERT_SECRET_", LookupEnv: os.LookupEnv}
	reference := cfg.Certificates.DNSTokenReference
	service, err := testfixtures.New(testfixtures.Config{
		DB: store, Audit: auditWriter, Token: cfg.Secrets.TestFixturesToken, Domain: cfg.TestFixtures.Domain, ZoneID: cfg.TestFixtures.ZoneID,
		DNS: &testfixtures.Cloudflare{HTTP: providerHTTPClient("cloudflare-test-fixtures", cfg.HTTP.RequestTimeout), Token: func(ctx context.Context) (string, error) {
			token, err := secrets.Resolve(ctx, reference)
			return string(token), err
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("configure test fixtures: %w", err)
	}
	return service, nil
}
