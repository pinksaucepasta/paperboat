package hosthealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var cert = json.RawMessage(`{"ok":false,"error":"x","stale_after_seconds":86400}`)

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
		{Source: "edge-certificate:\u212aexample.com", Payload: cert}, // Kelvin sign folds to k
		{Source: "edge-certificate:\u0130x.com", Payload: cert},       // dotted capital I folds to i
		{Source: "edge-certificate:\u0131x.com", Payload: cert},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"OK":true,"error":"","stale_after_seconds":86400}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":null,"error":"","stale_after_seconds":86400}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true,"error":"","stale_after_seconds":86400,"Stale_After_Seconds":0}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":"yes","error":"","stale_after_seconds":86400}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true,"error":1,"stale_after_seconds":86400}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true,"error":"","stale_after_seconds":"x"}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true,"error":"","stale_after_seconds":0}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true,"error":"","stale_after_seconds":1e9}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true,"error":"","stale_after_seconds":86400,"x":1}`)},
		{Source: "edge-certificate:x.com", Payload: json.RawMessage(`{"ok":true,"error":"` + strings.Repeat("a", 1025) + `","stale_after_seconds":86400}`)},
	} {
		if err := s.Ingest(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("report %.40s: err = %v, want ErrInvalid", bad.Payload, err)
		}
	}
	for _, source := range []string{"deploy", "edge-certificate:203.0.113.7", "edge-certificate:2001:db8::1"} {
		if err := s.Ingest(ctx, Report{Source: source, Payload: cert}); err != nil {
			t.Fatalf("source %s: %v", source, err)
		}
	}
	// A week-old edge row is dropped on the next edge report; past maxEdges a new edge is refused, a known one is not.
	if _, err := store.Pool().Exec(ctx, `UPDATE host_health_reports SET reported_at = now() - interval '8 days' WHERE source = 'edge-certificate:2001:db8::1'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxEdges-1; i++ {
		if err := s.Ingest(ctx, Report{Source: fmt.Sprintf("edge-certificate:e%d", i), Payload: cert}); err != nil {
			t.Fatalf("edge %d: %v", i, err)
		}
	}
	if err := s.Ingest(ctx, Report{Source: "edge-certificate:one-too-many", Payload: cert}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("edge past the bound: err = %v, want ErrInvalid", err)
	}
	if err := s.Ingest(ctx, Report{Source: "edge-certificate:203.0.113.7", Payload: cert}); err != nil {
		t.Fatalf("known edge at the bound: %v", err)
	}
	// At the bound, an edge switching to its node source is a new row and is refused; switching back to its
	// existing host row is an update and is accepted. With more fresh rows than the bound, known sources still update.
	if err := s.Ingest(ctx, Report{Source: "edge-certificate:node:0b6f1c1e-2f3a-4c5d-8e9f-0123456789ab", Payload: cert}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("new node source at the bound: err = %v, want ErrInvalid", err)
	}
	if _, err := store.Pool().Exec(ctx, `INSERT INTO host_health_reports(source,payload,reported_at) VALUES('edge-certificate:node:00000000-0000-4000-8000-000000000000','{}',now())`); err != nil {
		t.Fatal(err)
	}
	// A known source about to age out still updates at the bound (first: any other edge's report would retire it).
	if _, err := store.Pool().Exec(ctx, `UPDATE host_health_reports SET reported_at = now() - interval '8 days' WHERE source = 'edge-certificate:e1'`); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"edge-certificate:e1", "edge-certificate:e0", "edge-certificate:node:00000000-0000-4000-8000-000000000000"} {
		if err := s.Ingest(ctx, Report{Source: source, Payload: cert}); err != nil {
			t.Fatalf("known source %s past the bound: %v", source, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, `DELETE FROM host_health_reports WHERE source ~ '^edge-certificate:(e[0-9]+|node:.*)$'`); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Snapshot(ctx)
	if err != nil || len(entries) != 3 || entries[2].Source != "hosts" || string(entries[2].Payload) != `{"hosts": 2}` {
		t.Fatalf("snapshot = %+v, %v; want deploy, the current edge certificate row and the second hosts report", entries, err)
	}
	// Hostnames are case-insensitive: an edge row is stored lowercase, once, whatever case the report used.
	for _, source := range []string{"edge-certificate:Example.COM", "edge-certificate:example.com"} {
		if err := s.Ingest(ctx, Report{Source: source, Payload: cert}); err != nil {
			t.Fatal(err)
		}
		var sources []string
		rows, err := store.Pool().Query(ctx, `SELECT source FROM host_health_reports WHERE lower(source) = 'edge-certificate:example.com'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			sources = append(sources, v)
		}
		rows.Close()
		if len(sources) != 1 || sources[0] != "edge-certificate:example.com" {
			t.Fatalf("after %s: rows %v, want only the lowercase one", source, sources)
		}
	}
	// Concurrent new edges one below the bound: exactly one is admitted.
	if _, err := store.Pool().Exec(ctx, `DELETE FROM host_health_reports WHERE source LIKE 'edge-certificate:%';
INSERT INTO host_health_reports(source,payload,reported_at) SELECT 'edge-certificate:f'||g,'{}',now() FROM generate_series(1,63) g`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Ingest(ctx, Report{Source: fmt.Sprintf("edge-certificate:race%d", i), Payload: cert})
		}()
	}
	wg.Wait()
	var n int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM host_health_reports WHERE source LIKE 'edge-certificate:%'`).Scan(&n); err != nil || n != maxEdges {
		t.Fatalf("edge rows after concurrent inserts = %d, %v; want %d", n, err, maxEdges)
	}
}
