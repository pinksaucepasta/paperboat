package hosthealth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestAuthenticateNeedsTheConfiguredToken(t *testing.T) {
	token := strings.Repeat("t", 40)
	if !NewService(nil, token).Authenticate(token) || NewService(nil, token).Authenticate(token+"x") || NewService(nil, "").Authenticate("") {
		t.Fatal("only the configured token authenticates, and an empty config authenticates nothing")
	}
}

func TestLatestReportReplacesThePreviousOneAndBadReportsAreRefusedPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `DELETE FROM host_health_reports`); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, strings.Repeat("t", 40))
	for _, payload := range []string{`{"hosts":1}`, `{"hosts":2}`} {
		if err := s.Ingest(ctx, Report{Source: "hosts", Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []Report{
		{Source: "other", Payload: json.RawMessage(`{}`)},
		{Source: "edge-certificate:", Payload: json.RawMessage(`{}`)},
		{Source: "edge-certificate:a b", Payload: json.RawMessage(`{}`)},
		{Source: "edge-certificate:" + strings.Repeat("a", 254), Payload: json.RawMessage(`{}`)},
		{Source: "hosts", Payload: json.RawMessage(`[1]`)},
		{Source: "hosts", Payload: json.RawMessage(`null`)},
		{Source: "hosts", Payload: json.RawMessage(`{"x":"` + strings.Repeat("a", maxPayload) + `"}`)},
		{Source: "hosts"},
	} {
		if err := s.Ingest(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("report %.40s: err = %v, want ErrInvalid", bad.Payload, err)
		}
	}
	for _, source := range []string{"deploy", "edge-certificate:203.0.113.7", "edge-certificate:2001:db8::1"} {
		if err := s.Ingest(ctx, Report{Source: source, Payload: json.RawMessage(`{"ok":false}`)}); err != nil {
			t.Fatalf("source %s: %v", source, err)
		}
	}
	entries, err := s.Snapshot(ctx)
	if err != nil || len(entries) != 4 || entries[3].Source != "hosts" || string(entries[3].Payload) != `{"hosts": 2}` {
		t.Fatalf("snapshot = %+v, %v; want deploy, two edge certificate rows and the second hosts report", entries, err)
	}
}
