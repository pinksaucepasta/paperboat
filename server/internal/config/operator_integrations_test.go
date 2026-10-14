package config

import (
	"strings"
	"testing"
)

func TestOperatorIntegrationSecretsAndValidation(t *testing.T) {
	c := Default()
	c.Secrets.BackupReportToken = strings.Repeat("backup-private", 4)
	c.Secrets.SentryAPIToken = "sentry-private"
	c.Reporting.Organization = "paperboat"
	c.Reporting.Projects = []string{"server", "native"}
	if err := c.validateOperatorIntegrations(); err != nil {
		t.Fatal(err)
	}
	raw := c.RedactedJSON()
	if strings.Contains(raw, "backup-private") || strings.Contains(raw, "sentry-private") {
		t.Fatal("integration credential leaked")
	}
	c.Reporting.APIBaseURL = "https://user:secret@sentry.io"
	if c.validateOperatorIntegrations() == nil {
		t.Fatal("credential-bearing API URL allowed")
	}
	c.Reporting.APIBaseURL = ""
	c.Reporting.Projects = []string{"server", "server"}
	if c.validateOperatorIntegrations() == nil {
		t.Fatal("duplicate Sentry project allowed")
	}
	c.Reporting.Projects = []string{"server"}
	c.Backups.MaxAgeSeconds = -1
	if c.validateOperatorIntegrations() == nil {
		t.Fatal("negative freshness allowed")
	}
	c = Default()
	if err := c.validateOperatorIntegrations(); err != nil {
		t.Fatal("disabled config", err)
	}
}
