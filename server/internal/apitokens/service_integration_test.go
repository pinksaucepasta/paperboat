package apitokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

type fixture struct {
	store                *db.DB
	owner, member, other string
	team                 workspace.Scope
}

func openFixture(t *testing.T, ctx context.Context) fixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run api token integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	f := fixture{store: store, owner: "at_owner_" + suffix, member: "at_member_" + suffix, other: "at_other_" + suffix}
	f.team = workspace.Scope{ID: "at-" + suffix, TeamID: "at-" + suffix}
	for _, user := range []string{f.owner, f.member, f.other} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status)
VALUES($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, f.team.TeamID, f.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active)
VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, f.team.TeamID, f.owner, f.member); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAPITokenLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	f := openFixture(t, ctx)
	s := New(f.store, audit.NewWriter(f.store), config.APITokens{MaxLifetimeDays: 365, DefaultLifetimeDays: 90, MaxPerMember: 2, RetrySeconds: 600}, "test-encryption-key-0123456789abcdef")

	// Scope names are checked against the workspace's policy; the scope is stored as rows.
	var scopeErr *ScopeError
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "bad", Scope: json.RawMessage(`[{"dst":["tag:ci"],"allow":["exec"]}]`)}, ""); !errors.As(err, &scopeErr) || scopeErr.Errors[0].Code != "unknown_tag" {
		t.Fatalf("unknown tag in scope: %v", err)
	}
	token, secret, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "ci", Scope: json.RawMessage(`[{"dst":["machine:*"],"allow":["exec"]}]`)}, "")
	if err != nil {
		t.Fatal(err)
	}
	var rules int
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.api_token_scope_rules WHERE token_id=$1 AND target_kind='all_machines' AND action='exec'`, token.ID).Scan(&rules); err != nil || rules != 1 {
		t.Fatalf("scope rows %d %v", rules, err)
	}
	if token.CreatedBy != "user:"+f.member+"@example.test" || token.Workspace != f.team || len(token.Scope) != 1 {
		t.Fatalf("token = %+v", token)
	}
	if d := token.ExpiresAt.Sub(token.CreatedAt); d != 90*24*time.Hour {
		t.Fatalf("default lifetime %v", d)
	}
	var stored []byte
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT secret_hash FROM paperboat.api_tokens WHERE id=$1`, token.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(hashSecret(secret)) {
		t.Fatal("stored hash is not sha256(secret)")
	}

	p, err := s.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.TokenID != token.ID || p.User.ID != f.member || p.Workspace != f.team || len(p.Scope) != 1 || p.Scope[0].Allow[0] != policy.ActionExec {
		t.Fatalf("principal = %+v", p)
	}
	var first time.Time
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT last_used_at FROM paperboat.api_tokens WHERE id=$1`, token.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, secret); err != nil {
		t.Fatal(err)
	}
	var second time.Time
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT last_used_at FROM paperboat.api_tokens WHERE id=$1`, token.ID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if !second.Equal(first) {
		t.Fatal("last_used_at moved within a minute")
	}

	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "ci"}, ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, _, err := s.Create(ctx, f.member, workspace.Scope{ID: "personal"}, CreateInput{Name: "ci"}, ""); err != nil {
		t.Fatalf("same name in another workspace: %v", err)
	}
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "Bad Name"}, ""); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("bad name: %v", err)
	}
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "long", ExpiresInDays: 366}, ""); !errors.Is(err, ErrInvalidLifetime) {
		t.Fatalf("lifetime: %v", err)
	}
	scopeErr = nil
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "bad-scope", Scope: json.RawMessage(`[]`)}, ""); !errors.As(err, &scopeErr) {
		t.Fatalf("scope: %v", err)
	}
	if _, _, err := s.Create(ctx, f.other, f.team, CreateInput{Name: "x"}, ""); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("non-member: %v", err)
	}
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "second", ExpiresInDays: 1}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "third"}, ""); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
	ownerToken, _, err := s.Create(ctx, f.owner, f.team, CreateInput{Name: "owner"}, "")
	if err != nil {
		t.Fatal(err)
	}

	if list, err := s.List(ctx, f.member, f.team); err != nil || len(list) != 2 {
		t.Fatalf("member list: %d %v", len(list), err)
	}
	if list, err := s.List(ctx, f.owner, f.team); err != nil || len(list) != 3 {
		t.Fatalf("owner list: %d %v", len(list), err)
	}
	if _, err := s.Revoke(ctx, f.member, f.team, ownerToken.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member revokes owner's token: %v", err)
	}
	if _, err := s.Revoke(ctx, f.owner, workspace.Scope{ID: "personal"}, token.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke through another workspace: %v", err)
	}
	revoked, err := s.Revoke(ctx, f.owner, f.team, token.ID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("owner revokes member's token: %+v %v", revoked, err)
	}
	if again, err := s.Revoke(ctx, f.member, f.team, token.ID); err != nil || !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("revoke again: %v", err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked token authenticates: %v", err)
	}
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "ci"}, ""); err != nil {
		t.Fatalf("name reusable after revoke: %v", err)
	}
	var events int
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE resource_id=$1 AND event_type IN ('api_token.created','api_token.revoked')`, token.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("audit events = %d", events)
	}
}

func TestAPITokenAuthenticateRejectsExpiredAndInactive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	f := openFixture(t, ctx)
	s := New(f.store, audit.NewWriter(f.store), config.APITokens{MaxLifetimeDays: 365, DefaultLifetimeDays: 90, MaxPerMember: 10, RetrySeconds: 600}, "test-encryption-key-0123456789abcdef")
	if _, err := s.Authenticate(ctx, "pbat_unknown"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed: %v", err)
	}
	unknown, _, err := newSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, unknown); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown: %v", err)
	}
	expired, expiredSecret, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "expired"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.api_tokens SET created_at=now()-interval '2 days', expires_at=now()-interval '1 second' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, expiredSecret); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired: %v", err)
	}
	_, secret, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "live"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.users SET status='suspended' WHERE id=$1`, f.member); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrInvalid) {
		t.Fatalf("inactive creator: %v", err)
	}
}

// F21-12 (Lane X #16): a create retried with the same Idempotency-Key returns the same token and
// once-shown secret; the secret is held only encrypted and only for the retry window.
func TestAPITokenCreateRetryReturnsTheSameSecret(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	f := openFixture(t, ctx)
	s := New(f.store, audit.NewWriter(f.store), config.APITokens{MaxLifetimeDays: 365, DefaultLifetimeDays: 90, MaxPerMember: 5, RetrySeconds: 600}, "test-encryption-key-0123456789abcdef")
	in := CreateInput{Name: "deploy", ExpiresInDays: 30}
	first, secret, err := s.Create(ctx, f.member, f.team, in, "retry-key-0001")
	if err != nil {
		t.Fatal(err)
	}
	again, secretAgain, err := s.Create(ctx, f.member, f.team, in, "retry-key-0001")
	if err != nil || again.ID != first.ID || secretAgain != secret {
		t.Fatalf("retry: id %s/%s same secret %v err %v", first.ID, again.ID, secretAgain == secret, err)
	}
	var tokens int
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.api_tokens WHERE account_id=$1 AND name='deploy'`, f.member).Scan(&tokens); err != nil || tokens != 1 {
		t.Fatalf("tokens created = %d err=%v", tokens, err)
	}
	if _, _, err := s.Create(ctx, f.member, f.team, CreateInput{Name: "other"}, "retry-key-0001"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key, different request: %v", err)
	}
	if _, _, err := s.Create(ctx, f.member, f.team, in, "bad key"); !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("invalid key: %v", err)
	}
	var sealed []byte
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT secret_ciphertext FROM paperboat.api_token_create_requests WHERE account_id=$1 AND idempotency_key='retry-key-0001'`, f.member).Scan(&sealed); err != nil || strings.Contains(string(sealed), secret) {
		t.Fatalf("held secret is not encrypted (err %v)", err)
	}
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.api_token_create_requests SET created_at=now()-interval '11 minutes' WHERE account_id=$1`, f.member); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneCreateRequests(ctx); err != nil {
		t.Fatal(err)
	}
	var held int
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.api_token_create_requests WHERE account_id=$1`, f.member).Scan(&held); err != nil || held != 0 {
		t.Fatalf("held after the window = %d err=%v", held, err)
	}
	// After the window the key is free again; the name is taken, so a plain create says so.
	if _, _, err := s.Create(ctx, f.member, f.team, in, "retry-key-0001"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("after the window: %v", err)
	}
}
