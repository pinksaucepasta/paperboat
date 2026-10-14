package config

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestReportingDefaultsValidationAndRedaction(t *testing.T) {
	c := Default()
	if c.Reporting.Enabled {
		t.Fatal("reporting enabled by default")
	}
	c.Reporting.Enabled = true
	if c.Validate() == nil {
		t.Fatal("enabled without deployment config")
	}
	c.Secrets.SentryDSN = "https://sentry-secret@example.test/1"
	c.Reporting.Release = "server-commit"
	c.Reporting.IssueURL = "https://example.sentry.io/issues/"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.RedactedJSON(), "sentry-secret") {
		t.Fatal("DSN leaked in config")
	}
	c.Reporting.IssueURL = "https://example.sentry.io/issues/?token=secret"
	if c.Validate() == nil {
		t.Fatal("query accepted in operator link")
	}
	c.Reporting.IssueURL = ""
	c.Secrets.SentryDSN = "http://sentry-secret@example.test/1"
	if c.Validate() == nil {
		t.Fatal("plaintext reporting accepted")
	}
}

func TestEmbeddedReportingDefaultsAndOverrides(t *testing.T) {
	previousDSN, previousRelease := DefaultSentryDSN, DefaultSentryRelease
	t.Cleanup(func() {
		DefaultSentryDSN, DefaultSentryRelease = previousDSN, previousRelease
	})

	DefaultSentryDSN, DefaultSentryRelease = "", ""
	source := Default()
	if source.Reporting.Enabled || source.Secrets.SentryDSN != "" || source.Reporting.Release != "" {
		t.Fatal("source build reporting defaults were enabled")
	}

	DefaultSentryDSN = "https://official@example.test/1"
	DefaultSentryRelease = "paperboat-server:2026.09.19.1"
	official, err := Load(context.Background(), LoadOptions{LookupEnv: func(key string) (string, bool) {
		switch key {
		case "PAPERBOAT_SENTRY_ENABLED", "PAPERBOAT_SENTRY_DSN", "PAPERBOAT_SENTRY_RELEASE":
			return "", true
		}
		return "", false
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !official.Reporting.Enabled || official.Secrets.SentryDSN != DefaultSentryDSN || official.Reporting.Release != DefaultSentryRelease {
		t.Fatalf("official defaults lost: enabled=%v dsn=%q release=%q", official.Reporting.Enabled, official.Secrets.SentryDSN, official.Reporting.Release)
	}
	if err := official.Validate(); err != nil {
		t.Fatalf("official defaults are invalid: %v", err)
	}

	disabled, err := Load(context.Background(), LoadOptions{LookupEnv: mapLookup(map[string]string{"PAPERBOAT_SENTRY_ENABLED": "false"})})
	if err != nil || disabled.Reporting.Enabled {
		t.Fatalf("explicit disable was not honored: enabled=%v err=%v", disabled.Reporting.Enabled, err)
	}

	owned, err := Load(context.Background(), LoadOptions{LookupEnv: mapLookup(map[string]string{
		"PAPERBOAT_SENTRY_DSN":     "https://owned@example.test/2",
		"PAPERBOAT_SENTRY_RELEASE": "selfhost:1",
	})})
	if err != nil || owned.Secrets.SentryDSN != "https://owned@example.test/2" || owned.Reporting.Release != "selfhost:1" {
		t.Fatalf("deployment reporting override lost: dsn=%q release=%q err=%v", owned.Secrets.SentryDSN, owned.Reporting.Release, err)
	}
	if err := owned.Validate(); err != nil {
		t.Fatalf("deployment reporting override is invalid: %v", err)
	}

	secretFile, err := Load(context.Background(), LoadOptions{
		LookupEnv: mapLookup(map[string]string{"PAPERBOAT_SENTRY_DSN_FILE": "/run/secrets/sentry"}),
		ReadFile:  func(string) ([]byte, error) { return []byte("https://file@example.test/3\n"), nil },
	})
	if err != nil || secretFile.Secrets.SentryDSN != "https://file@example.test/3" {
		t.Fatalf("secret-file DSN override lost: dsn=%q err=%v", secretFile.Secrets.SentryDSN, err)
	}

	for _, partial := range []struct{ dsn, release string }{
		{dsn: "https://official@example.test/1"},
		{release: "paperboat-server:2026.09.19.1"},
	} {
		DefaultSentryDSN, DefaultSentryRelease = partial.dsn, partial.release
		cfg := Default()
		if cfg.Reporting.Enabled || cfg.Validate() == nil {
			t.Fatalf("partial embedded reporting configuration was accepted: dsn=%q release=%q", partial.dsn, partial.release)
		}
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// Run only on a linker-stamped acceptance binary; ordinary workspace tests skip it.
func TestOfficialLinkedReportingDefaults(t *testing.T) {
	if os.Getenv("PB_TEST_OFFICIAL_DEFAULTS") != "true" {
		t.Skip("requires explicitly stamped acceptance build")
	}
	if DefaultSentryDSN != "https://public@example.invalid/1" || DefaultSentryRelease != "paperboat-server:2026.09.19.0" {
		t.Fatal("official reporting defaults missing from binary")
	}
	cfg, err := Load(context.Background(), LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil || !cfg.Reporting.Enabled || cfg.Secrets.SentryDSN != DefaultSentryDSN || cfg.Reporting.Release != DefaultSentryRelease {
		t.Fatal("official binary did not enable embedded reporting destination")
	}
}
