package posthogquery

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestLivePostHogOverview reads real aggregates through the proxy. It runs only with
// PAPERBOAT_LIVE_POSTHOG_KEY_FILE (a personal key with query:read), PAPERBOAT_POSTHOG_HOST and
// PAPERBOAT_POSTHOG_PROJECT_ID set; it prints counts, never the key.
func TestLivePostHogOverview(t *testing.T) {
	keyFile := os.Getenv("PAPERBOAT_LIVE_POSTHOG_KEY_FILE")
	host, project := os.Getenv("PAPERBOAT_POSTHOG_HOST"), os.Getenv("PAPERBOAT_POSTHOG_PROJECT_ID")
	if keyFile == "" || host == "" || project == "" {
		t.Skip("set PAPERBOAT_LIVE_POSTHOG_KEY_FILE, PAPERBOAT_POSTHOG_HOST and PAPERBOAT_POSTHOG_PROJECT_ID")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := New(Config{Host: host, ProjectID: project, Key: strings.TrimSpace(string(key))}).Overview(context.Background(), 90)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("totals=%+v daily=%d pages=%v referrers=%v countries=%v devices=%v retention=%d errors=%v", got.Totals, len(got.Daily), got.TopPages, got.Referrers, got.Countries, got.Devices, len(got.Retention), got.Errors)
	if len(got.Errors) != 0 {
		t.Fatalf("insights failed against the live API: %v", got.Errors)
	}
}
