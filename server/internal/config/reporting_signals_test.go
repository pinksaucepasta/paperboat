package config

import (
	"context"
	"testing"
)

func TestReportingSignalConfiguration(t *testing.T) {
	for _, scenario := range []struct {
		name, value string
		invalid     bool
	}{
		{"PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", "0", false},
		{"PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", "1", false},
		{"PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", "NaN", true},
		{"PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", "1.01", true},
		{"PAPERBOAT_SENTRY_LOGS_ENABLED", "false", false},
		{"PAPERBOAT_SENTRY_METRICS_ENABLED", "false", false},
		{"PAPERBOAT_SENTRY_LOGS_ENABLED", "typo", true},
	} {
		cfg, err := Load(context.Background(), LoadOptions{LookupEnv: func(key string) (string, bool) {
			if key == scenario.name {
				return scenario.value, true
			}
			return "", false
		}})
		if (err != nil) != scenario.invalid {
			t.Fatalf("%s=%s: %v", scenario.name, scenario.value, err)
		}
		if err == nil && cfg.Reporting.Enabled {
			t.Fatal("a signal flag enabled reporting without consent")
		}
		if err == nil && scenario.name == "PAPERBOAT_SENTRY_LOGS_ENABLED" && (cfg.Reporting.LogsEnabled == nil || *cfg.Reporting.LogsEnabled) {
			t.Fatal("explicit disable lost")
		}
	}
}

func TestReportingFleetConfiguration(t *testing.T) {
	for _, scenario := range []struct {
		region, instance string
		invalid          bool
	}{
		{"", "", false}, {"mumbai", "slot-01", false}, {"eu-central", "slot-64", false},
		{"mumbai", "", true}, {"", "slot-01", true}, {"mumbai", "slot-65", true}, {"host.private.example", "slot-01", true},
	} {
		cfg, err := Load(context.Background(), LoadOptions{LookupEnv: func(key string) (string, bool) {
			switch key {
			case "PAPERBOAT_SENTRY_REGION":
				return scenario.region, true
			case "PAPERBOAT_SENTRY_INSTANCE":
				return scenario.instance, true
			}
			return "", false
		}})
		if err == nil {
			err = cfg.Validate()
		}
		if (err != nil) != scenario.invalid {
			t.Fatalf("fleet %q/%q err=%v", scenario.region, scenario.instance, err)
		}
		if err == nil && (cfg.Reporting.Region != scenario.region || cfg.Reporting.Instance != scenario.instance) {
			t.Fatal("fleet configuration lost")
		}
	}
}

func TestReportingHealthMonitorRequiresExplicitEnabledReporting(t *testing.T) {
	c := Default()
	c.Reporting.HealthMonitor = "paperboat-health"
	if c.Validate() == nil {
		t.Fatal("monitor enabled without reporting")
	}
	c.Reporting.Enabled = true
	c.Reporting.Release = "test"
	c.Secrets.SentryDSN = "https://public@example.test/1"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Reporting.HealthMonitor = "private.example.test"
	if c.Validate() == nil {
		t.Fatal("invalid monitor accepted")
	}
}

func TestWorkerStallTimeoutConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{"30m", true}, {"1m", true}, {"24h", true}, {"0", false}, {"30s", false}, {"25h", false}, {"private", false}} {
		c, err := Load(context.Background(), LoadOptions{LookupEnv: func(key string) (string, bool) {
			if key == "PAPERBOAT_WORKER_STALL_TIMEOUT" {
				return tc.value, true
			}
			return "", false
		}})
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.value, err)
		}
		if err == nil {
			if err = c.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
