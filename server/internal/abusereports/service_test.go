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
	s.ConfigureMail(config.TransactionalMail{From: "notices@example.test", AbuseMailbox: "abuse@mail.example.test"}, true)
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
	page, err := s.List(ctx, strings.ToUpper(host), "", "", time.Time{}, "", 10)
	reports := page.Reports
	if err != nil || page.Total != 2 || len(reports) != 2 || reports[0].ID != second.ID || reports[1].ID != first.ID || reports[0].Details != "drops an exe" {
		t.Fatalf("reports=%+v err=%v (the expired one must be gone)", reports, err)
	}
	// Every report alerts the abuse mailbox once, with reporter text escaped.
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.transactional_mail_outbox WHERE mail_id = ANY($1)`, []string{"abuse-report:" + first.ID, "abuse-report:" + second.ID})
	})
	var alerts int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.transactional_mail_outbox WHERE mail_id = ANY($1) AND payload->'to'->>0='abuse@mail.example.test'`, []string{"abuse-report:" + first.ID, "abuse-report:" + second.ID}).Scan(&alerts); err != nil || alerts != 2 {
		t.Fatalf("alerts=%d err=%v", alerts, err)
	}
	older, err := s.List(ctx, host, "", "", reports[0].CreatedAt, "", 10)
	if err != nil || len(older.Reports) != 1 || older.Reports[0].ID != first.ID {
		t.Fatalf("page before the newest=%+v err=%v", older, err)
	}
	// One report a page: the cursor carries the id, so the next page resumes exactly after the last row.
	one, err := s.List(ctx, host, "", "", time.Time{}, "", 1)
	if err != nil || len(one.Reports) != 1 || one.Reports[0].ID != second.ID || one.NextBefore == nil || one.NextBeforeID != second.ID {
		t.Fatalf("first single page=%+v err=%v", one, err)
	}
	at, id, err := ParseCursor(one.NextCursor)
	if err != nil || !at.Equal(*one.NextBefore) || id != one.NextBeforeID {
		t.Fatalf("cursor %q = %v %q %v", one.NextCursor, at, id, err)
	}
	rest, err := s.List(ctx, host, "", "", *one.NextBefore, one.NextBeforeID, 1)
	if err != nil || len(rest.Reports) != 1 || rest.Reports[0].ID != first.ID || rest.NextBefore != nil {
		t.Fatalf("second single page=%+v err=%v", rest, err)
	}
	// The reason filter is applied in the query, total included.
	malware, err := s.List(ctx, host, "malware", "", time.Time{}, "", 10)
	if err != nil || malware.Total != 1 || len(malware.Reports) != 1 || malware.Reports[0].ID != second.ID {
		t.Fatalf("malware only=%+v err=%v", malware, err)
	}
	if _, err := s.List(ctx, host, "nonsense", "", time.Time{}, "", 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown reason err=%v", err)
	}

	// Triage: an operations operator dismisses one report with a note; it leaves the open queue, says who and when,
	// and is audited with the reference. Reopening clears who and when. Support operators are refused.
	operator, support := "abuse_ops_"+host, "abuse_support_"+host
	for id, role := range map[string]string{operator: "operations", support: "support"} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active',$2)`, id, role); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.operator_accounts WHERE id IN ($1,$2)`, operator, support)
	})
	if open, err := s.List(ctx, host, "", "open", time.Time{}, "", 10); err != nil || open.Total != 2 || open.Reports[0].Status != "open" {
		t.Fatalf("new reports are open: %+v err=%v", open, err)
	}
	dismissed, err := s.Triage(ctx, operator, first.ID, "dismissed", " Not ours: a third-party host. ", "TICKET-7")
	if err != nil || dismissed.Status != "dismissed" || dismissed.TriagedBy != operator || dismissed.TriagedAt == nil || dismissed.TriageNote != "Not ours: a third-party host." {
		t.Fatalf("dismissed=%+v err=%v", dismissed, err)
	}
	if open, err := s.List(ctx, host, "", "open", time.Time{}, "", 10); err != nil || open.Total != 1 || open.Reports[0].ID != second.ID {
		t.Fatalf("open queue after the dismissal=%+v err=%v", open, err)
	}
	var audited int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE event_type='abuse_report.triaged' AND resource_id=$1 AND actor_user_id=$2 AND metadata->>'reference'='TICKET-7'`, first.ID, operator).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("triage audit rows=%d err=%v", audited, err)
	}
	reopened, err := s.Triage(ctx, operator, first.ID, "open", "back to the queue", "TICKET-8")
	if err != nil || reopened.Status != "open" || reopened.TriagedBy != "" || reopened.TriagedAt != nil || reopened.TriageNote != "" {
		t.Fatalf("reopened=%+v err=%v", reopened, err)
	}
	if _, err := s.Triage(ctx, support, first.ID, "dismissed", "", "TICKET-9"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("support triage err=%v", err)
	}
	if _, err := s.Triage(ctx, operator, "abuse_00000000-0000-4000-8000-000000000000", "dismissed", "", "TICKET-9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing report err=%v", err)
	}
	if _, err := s.Triage(ctx, operator, first.ID, "closed", "", "TICKET-9"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status err=%v", err)
	}
	if _, err := s.Triage(ctx, operator, first.ID, "dismissed", strings.Repeat("x", 1001), "TICKET-9"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-long note err=%v", err)
	}
	if _, err := s.List(ctx, host, "", "closed", time.Time{}, "", 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status filter err=%v", err)
	}
}

func TestAlertMessageEscapesReporterText(t *testing.T) {
	m := alertMessage(config.TransactionalMail{From: "n@example.test", AbuseMailbox: "abuse@mail.example.test"}, Report{ID: "abuse_x", Hostname: "a.example.test", URL: "https://a.example.test/<script>", Reason: "<i>phishing</i>", Details: "<b>hi</b>", ReporterEmail: "r@example.test"})
	if strings.Contains(m.HTML, "<script>") || strings.Contains(m.HTML, "<b>hi") || strings.Contains(m.HTML, "<i>") || m.To[0] != "abuse@mail.example.test" || !strings.Contains(m.Subject, "a.example.test") {
		t.Fatalf("alert = %+v", m)
	}
}
