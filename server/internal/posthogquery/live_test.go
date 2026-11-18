package posthogquery

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestLivePostHogInsights reads real aggregates through the proxy. It runs only with
// PAPERBOAT_LIVE_POSTHOG_KEY_FILE (a personal key with query:read), PAPERBOAT_POSTHOG_HOST and
// PAPERBOAT_POSTHOG_PROJECT_ID set; it prints counts, never the key.
func TestLivePostHogInsights(t *testing.T) {
	keyFile := os.Getenv("PAPERBOAT_LIVE_POSTHOG_KEY_FILE")
	host, project := os.Getenv("PAPERBOAT_POSTHOG_HOST"), os.Getenv("PAPERBOAT_POSTHOG_PROJECT_ID")
	if keyFile == "" || host == "" || project == "" {
		t.Skip("set PAPERBOAT_LIVE_POSTHOG_KEY_FILE, PAPERBOAT_POSTHOG_HOST and PAPERBOAT_POSTHOG_PROJECT_ID")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	service := New(Config{Host: host, ProjectID: project, Key: strings.TrimSpace(string(key))})
	site, err := service.Site(context.Background(), 90)
	if err != nil {
		t.Fatal(err)
	}
	console, err := service.Console(context.Background(), 90)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("site=%+v console=%+v", site, console)
	if len(site.Errors)+len(console.Errors) != 0 {
		t.Fatalf("insights failed against the live API: %v %v", site.Errors, console.Errors)
	}
}
