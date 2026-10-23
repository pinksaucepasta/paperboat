package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/testauth"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// Authenticated HTTP + actual PG owns invitation authorization, atomic enqueue,
// resend expiry/capacity and discovery. The local provider only observes requests
// and returns failures; durable retry state is produced by the real worker.
func TestInvitationMailPostgresHTTP(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	writer := audit.NewWriter(store)
	accounts := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"test-session-key"}, true, "https://login.example.test")
	cli := auth.NewDeviceService(store, writer, cfg.CLIAuth, []string{"test-cli-hash-key"})
	cli.ConfigureAuthorizationEncryptionKey("test-device-recovery-key")
	service := teams.NewService(store)
	router := authTestOriginHandler{next: NewRouter(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: accounts, DeviceAuth: cli, Teams: service})}
	suffix := fmt.Sprint(time.Now().UnixNano())
	identities := []string{"owner", "admin", "recipient", "outsider"}
	cookies := map[string][]*http.Cookie{}
	ids := map[string]string{}
	tokens := map[string]string{}
	for _, name := range identities {
		email := "mail-" + name + "-" + suffix + "@example.test"
		cookies[name] = loginCookies(t, router, "mail_"+name+suffix+":"+email+":"+name)
		ids[name] = userIDByEmail(t, store, email)
		tokens[name] = authorizeCLI(t, router, cookies[name]).AccessToken
	}
	team := "mail-http-" + suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`UPDATE paperboat.users SET email_verified=true WHERE id=ANY($1::text[])`, []string{ids["owner"], ids["admin"], ids["recipient"], ids["outsider"]})
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Team <script>unsafe</script>')`, team, ids["owner"])
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true)`, team, ids["owner"], ids["admin"])
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',8,now()-interval '1 hour',now()+interval '60 days')`, "bw_"+suffix, team)
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		tx, e := store.SQL().BeginTx(clean, nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		for _, q := range []string{`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`, `DELETE FROM paperboat.audit_events WHERE actor_user_id=ANY($1::text[])`, `DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, `DELETE FROM paperboat.teams WHERE team_id=$1`, `DELETE FROM paperboat.device_grants WHERE user_id=ANY($1::text[])`, `DELETE FROM paperboat.cli_client_sessions WHERE user_id=ANY($1::text[])`, `DELETE FROM paperboat.sessions WHERE user_id=ANY($1::text[])`, `DELETE FROM paperboat.user_identities WHERE user_id=ANY($1::text[])`, `DELETE FROM paperboat.users WHERE id=ANY($1::text[])`, `ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`} {
			var args []any
			if strings.Contains(q, "ANY") {
				args = []any{[]string{ids["owner"], ids["admin"], ids["recipient"], ids["outsider"]}}
			} else if strings.Contains(q, "team_id") {
				args = []any{team}
			}
			if _, e = tx.ExecContext(clean, q, args...); e != nil {
				t.Error(e)
				return
			}
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	})
	call := func(who, method, path string, body any, status int, out any) *httptest.ResponseRecorder {
		t.Helper()
		payload, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if method == "GET" {
			addCookies(req, cookies[who])
		} else if who != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[who])
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: status=%d want=%d", method, strings.Split(path, "?")[0], rec.Code, status)
		}
		if out != nil {
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if e = json.Unmarshal(rec.Body.Bytes(), &envelope); e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(envelope.Data, out); e != nil {
				t.Fatal(e)
			}
		}
		return rec
	}
	generation := func() uint64 {
		t.Helper()
		state, e := service.Get(ctx, ids["owner"], team)
		if e != nil {
			t.Fatal(e)
		}
		return state.Generation
	}
	invite := func(operation, email, role string) teams.Invitation {
		t.Helper()
		var v teams.Invitation
		call("owner", "POST", "/v1/teams/"+team+"/invitations", map[string]any{"operation_id": operation, "expected_generation": generation(), "email": email, "role": role}, 200, &v)
		return v
	}
	// An unconfigured sender creates a usable invitation without a fictitious send.
	first := invite("unconfigured", "before-signup-"+suffix+"@example.test", "member")
	var summary teams.InvitationSummary
	call("owner", "GET", "/v1/team-invitations/"+first.InvitationID, nil, 200, &summary)
	if summary.DeliveryState != "unconfigured" || summary.LastSentAt != nil {
		t.Fatal("unconfigured sender claimed a send")
	}
	var providerKeys, providerBodies []string
	var providerMu sync.Mutex
	snapshot := func() ([]string, []string) {
		providerMu.Lock()
		defer providerMu.Unlock()
		return append([]string(nil), providerKeys...), append([]string(nil), providerBodies...)
	}
	providerStatus := 503
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerMu.Lock()
		defer providerMu.Unlock()
		b, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
		}
		providerKeys = append(providerKeys, r.Header.Get("Idempotency-Key"))
		providerBodies = append(providerBodies, string(b))
		w.WriteHeader(providerStatus)
		if providerStatus == 200 {
			_, _ = io.WriteString(w, `{"id":"accepted-provider-id"}`)
		} else {
			_, _ = io.WriteString(w, `{"name":"service_unavailable"}`)
		}
	}))
	defer provider.Close()
	policy := cfg.Mail
	policy.From = "Paperboat <sender@example.test>"
	policy.ConsoleOrigin = "https://console.example.test"
	service.ConfigureMail(policy, true)
	worker := transactionalmail.Service{DB: store, Endpoint: provider.URL, Key: "test-mail-key"}
	second := invite("mail-invite", "mail-recipient-"+suffix+"@example.test", "member")
	call("owner", "GET", "/v1/team-invitations/"+second.InvitationID, nil, 200, &summary)
	if summary.DeliveryState != "queued" || summary.LastSentAt != nil {
		t.Fatal("queued message claimed provider acceptance")
	}
	// Invite operation replay cannot enqueue a second message, even with a stale generation.
	call("owner", "POST", "/v1/teams/"+team+"/invitations", map[string]any{"operation_id": "mail-invite", "expected_generation": second.Generation - 1, "email": "mail-recipient-" + suffix + "@example.test", "role": "member"}, 200, nil)
	var count int
	if e := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM transactional_mail_outbox WHERE invitation_id=$1`, second.InvitationID).Scan(&count); e != nil || count != 1 {
		t.Fatal("invite replay duplicated outbox")
	}
	if e := worker.Dispatch(ctx); e != nil {
		t.Fatal(e)
	}
	call("recipient", "GET", "/v1/team-invitations/"+second.InvitationID, nil, 200, &summary)
	if summary.LastSentAt != nil || summary.DeliveryState != "queued" {
		t.Fatal("failed attempt marked sent")
	}
	exec(`UPDATE paperboat.transactional_mail_outbox SET next_attempt_at=now() WHERE invitation_id=$1`, second.InvitationID)
	providerMu.Lock()
	providerStatus = 200
	providerMu.Unlock()
	if e := worker.Dispatch(ctx); e != nil {
		t.Fatal(e)
	}
	observedKeys, observedBodies := snapshot()
	if len(observedKeys) != 2 || observedKeys[0] != observedKeys[1] || observedBodies[0] != observedBodies[1] {
		t.Fatal("retry changed immutable provider request")
	}
	var rendered transactionalmail.Message
	if err = json.Unmarshal([]byte(observedBodies[0]), &rendered); err != nil {
		t.Fatal("invalid provider message JSON")
	}
	if strings.Contains(rendered.HTML, "<script>") || !strings.Contains(rendered.HTML, "&lt;script&gt;") || !strings.Contains(rendered.HTML, `href="`+policy.ConsoleOrigin+`/team"`) {
		t.Fatal("invitation rendering failed escaping or configured navigation")
	}
	call("recipient", "GET", "/v1/team-invitations/"+second.InvitationID, nil, 200, &summary)
	if summary.DeliveryState != "sent" || summary.LastSentAt == nil {
		t.Fatal("provider acceptance was not recorded")
	}
	sentAt := *summary.LastSentAt
	if e := worker.Dispatch(ctx); e != nil {
		t.Fatal(e)
	}
	if keys, _ := snapshot(); len(keys) != 2 {
		t.Fatal("accepted message sent twice")
	}
	var page struct {
		Items []teams.InvitationSummary `json:"items"`
	}
	call("owner", "GET", "/v1/team-invitations", nil, 200, &page)
	if len(page.Items) != 0 {
		t.Fatal("sender items leaked into inbox")
	}
	call("owner", "GET", "/v1/team-invitations?direction=sent", nil, 200, &page)
	if len(page.Items) != 2 {
		t.Fatal("sent inventory missing own invitations")
	}
	call("admin", "GET", "/v1/team-invitations?direction=sent", nil, 200, &page)
	if len(page.Items) != 0 {
		t.Fatal("sent inventory included another creator")
	}
	call("admin", "GET", "/v1/teams/"+team+"/invitations", nil, 200, &page)
	if len(page.Items) != 2 {
		t.Fatal("scoped inventory lost administrator visibility")
	}
	call("owner", "GET", "/v1/team-invitations?direction=unknown", nil, 400, nil)
	call("owner", "GET", "/v1/team-invitations?direction=sent&direction=received", nil, 400, nil)
	call("outsider", "GET", "/v1/team-invitations?direction=sent", nil, 200, &page)
	if len(page.Items) != 0 {
		t.Fatal("unauthorized email inventory leak")
	}
	call("outsider", "GET", "/v1/team-invitations/"+second.InvitationID, nil, 404, nil)
	resendPath := "/v1/teams/" + team + "/invitations/" + second.InvitationID + "/resend"
	call("outsider", "POST", resendPath, teams.AcceptRequest{OperationID: "outsider"}, 403, nil)
	call("recipient", "POST", resendPath, teams.AcceptRequest{OperationID: "recipient"}, 403, nil)
	call("owner", "POST", "/v1/teams/"+team+"/invitations/missing/resend", teams.AcceptRequest{OperationID: "missing"}, 404, nil)
	rec := call("owner", "POST", resendPath, teams.AcceptRequest{OperationID: "too-soon"}, 429, nil)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("cooldown omitted Retry-After")
	}
	exec(`UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, team, ids["owner"])
	call("outsider", "POST", resendPath, teams.AcceptRequest{OperationID: "hidden-suspension"}, 403, nil)
	call("owner", "POST", resendPath, teams.AcceptRequest{OperationID: "suspended"}, 409, nil)
	exec(`UPDATE paperboat.teams SET suspended=false,suspended_at=NULL,suspended_by=NULL WHERE team_id=$1`, team)
	exec(`UPDATE paperboat.team_invitations SET mail_requested_at=now()-interval '2 minutes',expires_at=now()-interval '1 day' WHERE invitation_id=$1`, second.InvitationID)
	exec(`UPDATE paperboat.billing_workspaces SET pending_seat_capacity=2 WHERE team_id=$1`, team)
	call("owner", "POST", resendPath, teams.AcceptRequest{OperationID: "no-seat"}, 409, nil)
	exec(`UPDATE paperboat.billing_workspaces SET pending_seat_capacity=NULL WHERE team_id=$1`, team)
	call("owner", "POST", resendPath, teams.AcceptRequest{OperationID: "resend"}, 200, &summary)
	if summary.DeliveryState != "queued" || summary.LastSentAt == nil || !summary.LastSentAt.Equal(sentAt) || time.Until(summary.ExpiresAt) < 30*24*time.Hour-time.Minute {
		t.Fatal("resend did not preserve accepted time and refresh 30 days")
	}
	refreshed := summary.ExpiresAt
	call("owner", "POST", resendPath, teams.AcceptRequest{OperationID: "resend"}, 200, &summary)
	if !summary.ExpiresAt.Equal(refreshed) {
		t.Fatal("resend replay refreshed expiry twice")
	}
	if e := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM transactional_mail_outbox WHERE invitation_id=$1`, second.InvitationID).Scan(&count); e != nil || count != 2 {
		t.Fatal("resend replay duplicated outbox")
	}

	// Removing sender configuration must not claim an earlier acceptance as the
	// result of the new request; its historical timestamp still remains accurate.
	service.ConfigureMail(policy, false)
	exec(`UPDATE paperboat.team_invitations SET mail_requested_at=now()-interval '2 minutes' WHERE invitation_id=$1`, second.InvitationID)
	call("owner", "POST", resendPath, teams.AcceptRequest{OperationID: "sender-disabled"}, 200, &summary)
	if summary.DeliveryState != "unconfigured" || summary.LastSentAt == nil || !summary.LastSentAt.Equal(sentAt) {
		t.Fatal("unconfigured resend reused old delivery status")
	}
	service.ConfigureMail(policy, true)
	// Cancelled invitations cannot refresh; queued obsolete mail is suppressed.
	call("owner", "POST", "/v1/teams/"+team+"/invitations/"+second.InvitationID+"/cancel", teams.MutationRequest{OperationID: "cancel", ExpectedGeneration: generation()}, 200, nil)
	call("owner", "POST", resendPath, teams.AcceptRequest{OperationID: "after-cancel"}, 409, nil)
	if e := worker.Dispatch(ctx); e != nil {
		t.Fatal(e)
	}
	if keys, _ := snapshot(); len(keys) != 2 {
		t.Fatal("cancelled invitation sent")
	}
	third := invite("admin-invite", "mail-outsider-"+suffix+"@example.test", "admin")
	adminPath := "/v1/teams/" + team + "/invitations/" + third.InvitationID + "/resend"
	call("admin", "POST", adminPath, teams.AcceptRequest{OperationID: "admin-denied"}, 403, nil)
	exec(`UPDATE paperboat.team_invitations SET mail_requested_at=now()-interval '2 minutes' WHERE invitation_id=$1`, third.InvitationID)
	call("owner", "POST", adminPath, teams.AcceptRequest{OperationID: "owner-resend"}, 200, nil)

	// A former owner remains an admin: the owner-only stored-role rule, rather
	// than ordinary membership loss, must reject replay while the invite is pending.
	exec(`UPDATE paperboat.teams SET owner_account=$2 WHERE team_id=$1`, team, ids["admin"])
	exec(`UPDATE paperboat.team_members SET role='admin' WHERE team_id=$1 AND account_id=$2`, team, ids["owner"])
	call("owner", "POST", adminPath, teams.AcceptRequest{OperationID: "owner-resend"}, 403, nil)
	call("outsider", "POST", "/v1/team-invitations/"+third.InvitationID+"/accept", teams.AcceptRequest{OperationID: "accept"}, 200, nil)
	call("admin", "POST", adminPath, teams.AcceptRequest{OperationID: "after-accept"}, 409, nil)
}
