package auditstream

import (
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/activity"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func testStore(t *testing.T) *db.DB {
	t.Helper()
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
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	return store
}

// PLAN F19-10: configure, deliver in order with a valid signature, back off and alert on failure,
// and upload gzip NDJSON objects signed with SigV4.
func TestPostgresAuditStreamDelivery(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, member, team := "as_owner_"+x, "as_member_"+x, "as-"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,email_verified) VALUES($1,$1,$1||'@example.test','active',true),($2,$2,$2||'@example.test','active',true)`, owner, member)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Stream team')`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,quantity,period_start,period_end) VALUES('team:'||$1,$1,'active',2,now()-interval '1 day',now()+interval '30 days')`, team)

	var mu sync.Mutex
	var bodies [][]byte
	var headers []http.Header
	fail := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, _ := gzip.NewReader(strings.NewReader(string(body)))
			body, _ = io.ReadAll(zr)
		}
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(503)
			return
		}
		bodies, headers = append(bodies, body), append(headers, r.Header.Clone())
	}))
	defer srv.Close()
	log := activity.New(store, config.Default().Business)
	s := New(store, log, config.Default().Business, "test-encryption-key-0123456789abcdef", config.TransactionalMail{From: "a@example.test", ConsoleOrigin: "https://console.example.test"}, true)
	s.client = func() httpDoer { return srv.Client() }
	clock := time.Now()
	s.now = func() time.Time { return clock }

	if _, err := s.Configure(ctx, member, team, "https", Input{URL: "https://siem.example.test/hook"}); err != ErrForbidden {
		t.Fatalf("member configured a stream: %v", err)
	}
	d, err := s.Configure(ctx, owner, team, "https", Input{URL: "https://siem.example.test/hook"})
	if err != nil || !strings.HasPrefix(d.SigningSecret, "pbsig_") {
		t.Fatalf("configure = %+v err=%v", d, err)
	}
	// Editing: a newly added header needs its value; omitted secrets keep the stored ones.
	hook := Input{URL: "https://siem.example.test/hook", HeaderName: "Authorization"}
	if _, err = s.Configure(ctx, owner, team, "https", hook); err != ErrInvalid {
		t.Fatalf("header without a value: %v", err)
	}
	hook.HeaderValue = "Splunk token"
	if _, err = s.Configure(ctx, owner, team, "https", hook); err != nil {
		t.Fatal(err)
	}
	hook.HeaderValue = ""
	if edited, err := s.Configure(ctx, owner, team, "https", hook); err != nil || edited.SigningSecret != "" {
		t.Fatalf("edit without secrets = %+v err=%v", edited, err)
	}
	exec(`UPDATE paperboat.audit_stream_destinations SET url=$2 WHERE team_id=$1`, team, srv.URL)
	w := audit.NewWriter(store)
	for i := range 3 {
		if err = w.Write(ctx, audit.Event{ActorUserID: owner, ActorType: audit.ActorUser, EventType: fmt.Sprintf("team.event_%d", i), ResourceType: "team", ResourceID: team}); err != nil {
			t.Fatal(err)
		}
	}
	exec(`SELECT 1`) // events are older than the settle window only after it passes:
	exec(`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`)
	exec(`UPDATE paperboat.audit_events SET created_at=created_at-interval '1 minute' WHERE activity_workspace_id='team:'||$1`, team)
	exec(`ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`)
	if err = s.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(bodies) != 1 || strings.Count(string(bodies[0]), "\n") < 3 || headers[0].Get("Authorization") != "Splunk token" {
		t.Fatalf("delivered %d batches: %q", len(bodies), bodies)
	}
	parts := strings.SplitN(strings.TrimPrefix(headers[0].Get("Paperboat-Signature"), "t="), ",v1=", 2)
	m := hmac.New(sha256.New, []byte(d.SigningSecret))
	m.Write([]byte(parts[0] + "."))
	m.Write(bodies[0])
	if len(parts) != 2 || parts[1] != hex.EncodeToString(m.Sum(nil)) {
		t.Fatal("signature does not verify")
	}
	if !strings.Contains(string(bodies[0]), `"action":"team.event_0"`) || strings.Index(string(bodies[0]), `"team.event_0"`) > strings.Index(string(bodies[0]), `"team.event_2"`) {
		t.Fatal("events missing or out of order")
	}
	fail = true
	mu.Unlock()

	if err = w.Write(ctx, audit.Event{ActorUserID: owner, ActorType: audit.ActorUser, EventType: "team.event_late", ResourceType: "team", ResourceID: team}); err != nil {
		t.Fatal(err)
	}
	exec(`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`)
	exec(`UPDATE paperboat.audit_events SET created_at=now()-interval '1 minute' WHERE event_type='team.event_late' AND resource_id=$1`, team)
	exec(`ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`)
	if err = s.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	exec(`UPDATE paperboat.audit_stream_destinations SET next_attempt_at=now(),failing_since=now()-interval '2 hours' WHERE team_id=$1`, team)
	if err = s.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, owner, team)
	if err != nil || len(list) != 1 || list[0].LastError == "" || list[0].Pending < 1 {
		t.Fatalf("status = %+v err=%v", list, err)
	}
	var alerts, mails int
	_ = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_id=$1 AND event_type='team.activity_stream_failing'`, team).Scan(&alerts)
	_ = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE 'audit-stream-failing:'||$1||':%'`, team).Scan(&mails)
	if alerts != 1 || mails != 1 {
		t.Fatalf("alerts=%d mails=%d", alerts, mails)
	}

	mu.Lock()
	fail, bodies, headers = false, nil, nil
	mu.Unlock()
	if _, err = s.Configure(ctx, owner, team, "s3", Input{URL: "https://s3.example.test", Region: "eu-central-1", Bucket: "audit", Prefix: "pb", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE paperboat.audit_stream_destinations SET url=$2,cursor_sequence=0 WHERE team_id=$1 AND kind='s3'`, team, srv.URL)
	if err = s.Test(ctx, owner, team, "s3"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 1 || !strings.HasPrefix(headers[0].Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/") || !strings.Contains(string(bodies[0]), "team.activity_stream_test") {
		t.Fatalf("s3 upload headers=%v body=%q", headers, bodies)
	}
}
