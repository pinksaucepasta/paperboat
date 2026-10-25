package db

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Session state and migrations use the direct DSN; queries keep the pooled one. Opening does not connect.
func TestOpenSplitsPooledAndDirectDSN(t *testing.T) {
	cfg := config.Database{Driver: "postgres", DSN: "postgres://app:secret@pooler.example.test:6432/paperboat", DirectDSN: "postgres://app:secret@primary.example.test:5432/paperboat"}
	store, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := store.Pool().Config().ConnConfig; got.Host != "pooler.example.test" || got.Port != 6432 || store.Pool().Config().MaxConns != 20 {
		t.Fatalf("pooled pool = %s:%d max %d, want the pooled DSN with 20 connections", got.Host, got.Port, store.Pool().Config().MaxConns)
	}
	if got := store.Direct().Config().ConnConfig; got.Host != "primary.example.test" || got.Port != 5432 || store.Direct().Config().MaxConns != directMaxConns {
		t.Fatalf("direct pool = %s:%d max %d, want the direct DSN with %d connections", got.Host, got.Port, store.Direct().Config().MaxConns, directMaxConns)
	}
	if got := store.migrationConfig.ConnConfig; got.Host != "primary.example.test" || got.Port != 5432 {
		t.Fatalf("migrations connect to %s:%d, want the direct DSN", got.Host, got.Port)
	}
}

// An empty direct DSN means the same DSN: a single-host deployment keeps one endpoint.
func TestOpenWithoutDirectDSNUsesPooledDSN(t *testing.T) {
	store, err := Open(config.Database{Driver: "postgres", DSN: "postgres://app:secret@db.example.test:5432/paperboat"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := store.Direct().Config().ConnConfig; got.Host != "db.example.test" || got.Port != 5432 {
		t.Fatalf("direct pool = %s:%d, want the pooled DSN", got.Host, got.Port)
	}
}
