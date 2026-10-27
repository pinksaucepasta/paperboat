package config

import (
	"strings"
	"testing"
)

func TestTestFixturesConfig(t *testing.T) {
	token := strings.Repeat("k", 40)
	set := func(env map[string]string) Config {
		c := Default()
		lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
		if err := overlayEnv(&c, lookup, nil); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := set(nil).validateOperatorIntegrations(); err != nil {
		t.Fatalf("no test fixtures configured must be valid: %v", err)
	}
	ok := set(map[string]string{"PAPERBOAT_TEST_FIXTURES_TOKEN": token, "PAPERBOAT_TEST_FIXTURES_DOMAIN": "smoke.example.test", "PAPERBOAT_TEST_FIXTURES_ZONE_ID": "zone123"})
	if err := ok.validateOperatorIntegrations(); err != nil || ok.Secrets.TestFixturesToken != token || ok.TestFixtures.Domain != "smoke.example.test" {
		t.Fatalf("complete test fixtures config: %v %+v", err, ok.TestFixtures)
	}
	if err := set(map[string]string{"PAPERBOAT_TEST_FIXTURES_TOKEN": token}).validateOperatorIntegrations(); err != nil {
		t.Fatalf("team fixtures alone (no DNS) must be valid: %v", err)
	}
	for name, env := range map[string]map[string]string{
		"short token":     {"PAPERBOAT_TEST_FIXTURES_TOKEN": "short"},
		"domain no token": {"PAPERBOAT_TEST_FIXTURES_DOMAIN": "smoke.example.test", "PAPERBOAT_TEST_FIXTURES_ZONE_ID": "z"},
		"domain no zone":  {"PAPERBOAT_TEST_FIXTURES_TOKEN": token, "PAPERBOAT_TEST_FIXTURES_DOMAIN": "smoke.example.test"},
		"zone no domain":  {"PAPERBOAT_TEST_FIXTURES_TOKEN": token, "PAPERBOAT_TEST_FIXTURES_ZONE_ID": "z"},
		"invalid domain":  {"PAPERBOAT_TEST_FIXTURES_TOKEN": token, "PAPERBOAT_TEST_FIXTURES_DOMAIN": "not a domain", "PAPERBOAT_TEST_FIXTURES_ZONE_ID": "z"},
	} {
		if err := set(env).validateOperatorIntegrations(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if out := ok.RedactedJSON(); strings.Contains(out, token) || !strings.Contains(out, "[redacted]") {
		t.Errorf("the test fixtures token must be redacted in the logged configuration")
	}
}
