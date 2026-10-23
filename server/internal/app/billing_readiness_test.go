package app

import (
	"context"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestReadinessDoesNotRequireRetiredOrUnconfiguredBilling(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Default()
	cfg.Environment = config.EnvironmentProduction
	cfg.ReleaseDirectory = ""
	cfg.Secrets.WorkOSClientID = "client_test"
	cfg.Secrets.WorkOSAPIKey = "sk_test_configured"
	r := readinessChecker{cfg: cfg, db: store}
	if err := r.Ready(context.Background()); err != nil {
		t.Fatalf("optional payment setup blocked personal runtime: %v", err)
	}
	r.cfg.Secrets.WorkOSAPIKey = ""
	if err := r.Ready(context.Background()); err == nil {
		t.Fatal("missing required identity provider was ignored")
	}
}
