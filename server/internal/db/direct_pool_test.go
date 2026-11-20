package db

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Session state and migrations use the direct DSN; queries keep the pooled one. Open looks up max_connections to size the pools, so an unreachable host only keeps the configured sizes.
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

// startup_params=false is the pooler setting: no startup parameters and no named prepared statements on the pooled
// pool, which PgBouncer in transaction mode cannot honour (F21-15). The direct pool and the default keep both.
func TestPoolerModeSendsNoStartupParamsAndUsesUnnamedStatements(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name       string
		params     *bool
		wantMode   pgx.QueryExecMode
		wantParams bool
	}{{"default", nil, pgx.QueryExecModeCacheStatement, true}, {"pooler", &off, pgx.QueryExecModeCacheDescribe, false}} {
		store, err := Open(config.Database{Driver: "postgres", DSN: "postgres://app:secret@pooler.example.test:6432/paperboat", StartupParams: tc.params})
		if err != nil {
			t.Fatal(err)
		}
		got := store.Pool().Config().ConnConfig
		_, hasParams := got.RuntimeParams["search_path"]
		if got.DefaultQueryExecMode != tc.wantMode || hasParams != tc.wantParams {
			t.Errorf("%s: exec mode %v, search_path startup param %v; want %v, %v", tc.name, got.DefaultQueryExecMode, hasParams, tc.wantMode, tc.wantParams)
		}
		store.Close()
	}
}

// A retired server keeps serving held streams after a rollout and stops using most of its connections; they must go
// back to Postgres quickly (pgx keeps an idle connection 30 minutes by default) or four overlapping instances of 26
// connections each exceed max_connections 100.
func TestPoolsReleaseIdleConnectionsWithinMinutes(t *testing.T) {
	store, err := Open(config.Database{Driver: "postgres", DSN: "postgres://app:secret@primary.example.test:5432/paperboat"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for name, idle := range map[string]time.Duration{"request": store.Pool().Config().MaxConnIdleTime, "direct": store.Direct().Config().MaxConnIdleTime} {
		if idle <= 0 || idle > 5*time.Minute {
			t.Fatalf("%s pool keeps an idle connection for %v, want at most 5 minutes", name, idle)
		}
	}
}
