package supportreport

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestLiveSentryDeploymentProbe is opt-in: only deployment verification supplies
// its configured destination. It submits synthetic, content-free support evidence.
func TestLiveSentryDeploymentProbe(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_SENTRY_DSN")
	if dsn == "" {
		t.Skip("deployment Sentry destination not supplied")
	}
	release := os.Getenv("PAPERBOAT_TEST_SENTRY_RELEASE")
	store := &memoryStore{}
	service, err := New(store, Config{Enabled: true, DSN: dsn, Release: release, Environment: "production"})
	if err != nil {
		t.Fatal("deployment reporting configuration rejected")
	}
	ctx := service.Context(context.Background(), NewReference())
	service.CaptureFailure(ctx, "paperboat-server", "http_billing", "http_handler", "internal_error", errors.New("synthetic deployment check"))
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Run(runCtx); err != nil {
		t.Fatal(err)
	}
	service.Close()
	if len(store.records) != 1 || store.records[0].EventID == "" {
		t.Fatal("synthetic support evidence was not submitted")
	}
	if service.FlushStatus() != "drained" || service.SDKHTTPFailures() != 0 || service.SDKSubmissionsDropped() != 0 {
		t.Fatal("deployment reporting did not drain successfully")
	}
}
