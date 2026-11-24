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
	s := NewService(store)
	for _, payload := range []string{`{"hosts":1}`, `{"hosts":2}`} {
		if err := s.Ingest(ctx, Report{Source: "hosts", Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []Report{
		{Source: "other", Payload: json.RawMessage(`{}`)},
		{Source: "hosts", Payload: json.RawMessage(`[1]`)},
		{Source: "hosts", Payload: json.RawMessage(`null`)},
		{Source: "hosts", Payload: json.RawMessage(`{"x":"` + strings.Repeat("a", maxPayload) + `"}`)},
		{Source: "hosts"},
	} {
		if err := s.Ingest(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("report %.40s: err = %v, want ErrInvalid", bad.Payload, err)
		}
	}
	if err := s.Ingest(ctx, Report{Source: "deploy", Payload: json.RawMessage(`{"ok":false}`)}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Source != "deploy" || entries[1].Source != "hosts" || string(entries[1].Payload) != `{"hosts": 2}` {
		t.Fatalf("snapshot = %+v; want deploy and the second hosts report", entries)
	}
}
