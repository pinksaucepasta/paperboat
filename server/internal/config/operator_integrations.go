package config

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var operatorIntegrationID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (c Config) validateOperatorIntegrations() error {
	r := c.Reporting
	if r.APIBaseURL != "" || r.Organization != "" || len(r.Projects) != 0 || c.Secrets.SentryAPIToken != "" {
		base := r.APIBaseURL
		if base == "" {
			base = "https://sentry.io"
		}
		u, e := url.Parse(base)
		seen := make(map[string]struct{}, len(r.Projects))
		projectsOK := len(r.Projects) >= 1 && len(r.Projects) <= 8
		for _, project := range r.Projects {
			_, duplicate := seen[project]
			if !operatorIntegrationID.MatchString(project) || strings.TrimSpace(project) != project || duplicate {
				projectsOK = false
			}
			seen[project] = struct{}{}
		}
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || !operatorIntegrationID.MatchString(r.Organization) || !projectsOK || len(c.Secrets.SentryAPIToken) < 1 || len(c.Secrets.SentryAPIToken) > 4096 {
			return errors.New("Sentry admin API requires an HTTPS origin, organization slug, 1–8 unique project slugs and API token")
		}
	}
	if c.Backups.DatabaseID != "" && !operatorIntegrationID.MatchString(c.Backups.DatabaseID) || c.Secrets.BackupReportToken != "" && (len(c.Secrets.BackupReportToken) < 32 || len(c.Secrets.BackupReportToken) > 512) {
		return errors.New("backup reporting requires a valid database ID and a token of 32–512 bytes")
	}
	if token := c.Secrets.TestFixturesToken; token != "" && (len(token) < 32 || len(token) > 512) || (c.TestFixtures.Domain != "" || c.TestFixtures.ZoneID != "") && (c.Secrets.TestFixturesToken == "" || !validEndpointBaseDomain(c.TestFixtures.Domain) || !validConfigMetadata(c.TestFixtures.ZoneID, 128)) {
		return errors.New("test fixtures need a token of 32–512 bytes, and test DNS a valid domain and zone id together with it")
	}
	if n := c.Backups.MaxAgeSeconds; n != 0 && (n < 60 || n > 30*24*3600) {
		return errors.New("backup maximum age must be 60–2592000 seconds")
	}
	if n := c.Backups.RestoreMaxAgeSeconds; n != 0 && (n < 60 || n > 90*24*3600) {
		return errors.New("restore verification maximum age must be 60–7776000 seconds")
	}
	return nil
}
