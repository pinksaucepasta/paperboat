package db

import (
	"context"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
)

// JIT compilation made the machine inventory take seconds (the compile alone was ~1 s for an 18 ms query), so the
// server's connections run with it off.
func TestPoolConnectionsRunWithJITOff(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var jit string
	if err = database.Pool().QueryRow(context.Background(), `SHOW jit`).Scan(&jit); err != nil || jit != "off" {
		t.Fatalf("jit=%q err=%v, want off", jit, err)
	}
}
