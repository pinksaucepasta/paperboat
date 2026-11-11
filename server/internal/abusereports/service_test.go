package abusereports

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestValidateNormalizesAndNamesTheBadField(t *testing.T) {
	in, host, err := validate(Input{URL: " https://Login-Bank.Example.Test./verify?x=1 ", Reason: "phishing", Details: "line one\nline two", ReporterEmail: "me@example.test"})
	if err != nil || host != "login-bank.example.test" || in.URL != "https://Login-Bank.Example.Test./verify?x=1" {
		t.Fatalf("valid report: %+v %q %v", in, host, err)
	}
	for name, bad := range map[string]Input{
		"url":      {URL: "javascript:alert(1)", Reason: "phishing"},
		"userinfo": {URL: "https://user:pass@example.test/", Reason: "phishing"},
		"reason":   {URL: "https://a.example.test/", Reason: "boring"},
		"details":  {URL: "https://a.example.test/", Reason: "spam", Details: strings.Repeat("x", 4001)},
		"email":    {URL: "https://a.example.test/", Reason: "spam", ReporterEmail: "a@b.test, c@d.test"},
		"control":  {URL: "https://a.example.test/\x00", Reason: "spam"},
	} {
		if _, _, err := validate(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func testStore(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

// Reports are stored, listed newest first per hostname, and pruned past 180 days by the writer.
func TestSubmitListAndRetentionOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	host := fmt.Sprintf("abuse-%d.example.test", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.abuse_reports WHERE hostname=$1`, host)
	})
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	s := New(store)
	s.now = func() time.Time { return now }
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.abuse_reports(id,hostname,url,reason,created_at) VALUES('abuse_00000000-0000-4000-8000-'||lpad((extract(epoch from now())*1000)::bigint::text,12,'0'),$1,'https://'||$1,'spam',$2)`, host, now.Add(-Retention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	first, err := s.Submit(ctx, Input{URL: "https://" + host + "/a", Reason: "phishing"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second, err := s.Submit(ctx, Input{URL: "https://" + host + "/b", Reason: "malware", Details: "drops an exe", ReporterEmail: "r@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	reports, err := s.List(ctx, strings.ToUpper(host), time.Time{}, 10)
	if err != nil || len(reports) != 2 || reports[0].ID != second.ID || reports[1].ID != first.ID || reports[0].Details != "drops an exe" {
		t.Fatalf("reports=%+v err=%v (the expired one must be gone)", reports, err)
	}
	older, err := s.List(ctx, host, reports[0].CreatedAt, 10)
	if err != nil || len(older) != 1 || older[0].ID != first.ID {
		t.Fatalf("page before the newest=%+v err=%v", older, err)
	}
}
