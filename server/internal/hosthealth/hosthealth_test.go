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
	// Edge rows never come from here: each edge's certificate renewal rides in its own heartbeat.
	for _, source := range []string{"edge-certificate:203.0.113.7", "edge-certificate:node-edge-a"} {
		if err := s.Ingest(ctx, Report{Source: source, Payload: json.RawMessage(`{"ok":true}`)}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("source %s: err = %v, want ErrInvalid", source, err)
		}
	}
	if err := s.Ingest(ctx, Report{Source: "deploy", Payload: json.RawMessage(`{"ok":false}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `INSERT INTO host_health_reports(source,payload) VALUES('edge-certificate:example.com','{}')`); err == nil {
		t.Fatal("the table accepted an edge row")
	}
	entries, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var operator []Entry
	for _, e := range entries {
		if !strings.HasPrefix(e.Source, "edge-certificate:") {
			operator = append(operator, e)
		}
	}
	if len(operator) != 2 || operator[0].Source != "deploy" || operator[1].Source != "hosts" || string(operator[1].Payload) != `{"hosts": 2}` {
		t.Fatalf("snapshot = %+v; want deploy and the second hosts report", operator)
	}
}
