package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostHogAnalyticsConfig(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("phx_secret_value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func(env map[string]string) (Config, error) {
		c := Default()
		err := overlayEnv(&c, func(n string) (string, bool) { v, ok := env[n]; return v, ok }, os.ReadFile)
		return c, err
	}
	if c, err := load(map[string]string{"PAPERBOAT_POSTHOG_HOST": "https://posthog.example.test", "PAPERBOAT_POSTHOG_PROJECT_ID": "@current", "PAPERBOAT_POSTHOG_PERSONAL_API_KEY_FILE": keyFile}); err != nil || c.validateOperatorIntegrations() != nil {
		t.Fatalf("@current must be accepted: %v", err)
	}
	good := map[string]string{"PAPERBOAT_POSTHOG_HOST": "https://posthog.example.test", "PAPERBOAT_POSTHOG_PROJECT_ID": "123", "PAPERBOAT_POSTHOG_PERSONAL_API_KEY_FILE": keyFile}
	c, err := load(good)
	if err != nil || c.Secrets.PostHogPersonalKey != "phx_secret_value" || c.Analytics.PostHogProjectID != "123" {
		t.Fatalf("load: %v %+v", err, c.Analytics)
	}
	if err := c.validateOperatorIntegrations(); err != nil {
		t.Fatalf("complete config: %v", err)
	}
	if out := c.RedactedJSON(); strings.Contains(out, "phx_secret_value") {
		t.Fatal("the PostHog key must be redacted in the logged configuration")
	}
	if c, _ = load(nil); c.validateOperatorIntegrations() != nil {
		t.Fatal("no analytics configured must be valid")
	}
	for name, mutate := range map[string]func(map[string]string){
		"no host":        func(e map[string]string) { delete(e, "PAPERBOAT_POSTHOG_HOST") },
		"no project":     func(e map[string]string) { delete(e, "PAPERBOAT_POSTHOG_PROJECT_ID") },
		"no key":         func(e map[string]string) { delete(e, "PAPERBOAT_POSTHOG_PERSONAL_API_KEY_FILE") },
		"plain http":     func(e map[string]string) { e["PAPERBOAT_POSTHOG_HOST"] = "http://posthog.example.test" },
		"host with path": func(e map[string]string) { e["PAPERBOAT_POSTHOG_HOST"] = "https://posthog.example.test/api" },
		"host userinfo":  func(e map[string]string) { e["PAPERBOAT_POSTHOG_HOST"] = "https://u:p@posthog.example.test" },
		"project slash":  func(e map[string]string) { e["PAPERBOAT_POSTHOG_PROJECT_ID"] = "1/../2" },
	} {
		env := map[string]string{}
		for k, v := range good {
			env[k] = v
		}
		mutate(env)
		c, err := load(env)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.validateOperatorIntegrations() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPostHogCaptureConfig(t *testing.T) {
	load := func(env map[string]string) (Config, error) {
		c := Default()
		err := overlayEnv(&c, func(n string) (string, bool) { v, ok := env[n]; return v, ok }, os.ReadFile)
		return c, err
	}
	for name, env := range map[string]map[string]string{
		"host without key": {"PAPERBOAT_POSTHOG_CAPTURE_HOST": "https://ingest.example.test"},
		"key without host": {"PAPERBOAT_POSTHOG_CAPTURE_KEY": "phc_public"},
		"plain http":       {"PAPERBOAT_POSTHOG_CAPTURE_HOST": "http://ingest.example.test", "PAPERBOAT_POSTHOG_CAPTURE_KEY": "phc_public"},
	} {
		if c, err := load(env); err != nil || c.validateOperatorIntegrations() == nil {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	c, err := load(map[string]string{"PAPERBOAT_POSTHOG_CAPTURE_HOST": "https://ingest.example.test", "PAPERBOAT_POSTHOG_CAPTURE_KEY": "phc_public"})
	if err != nil || c.validateOperatorIntegrations() != nil || c.Analytics.PostHogCaptureKey != "phc_public" {
		t.Fatalf("a capture host and key together are valid: %v", err)
	}
}
