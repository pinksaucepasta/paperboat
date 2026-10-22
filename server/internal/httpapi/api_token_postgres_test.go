package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/apitokens"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/favorites"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

func TestAPITokenBearerRequests(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, team := "apit_owner_"+suffix, "apit_member_"+suffix, "apit-"+suffix
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, owner, member)
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, member)
	writer := audit.NewWriter(store)
	tokens := apitokens.New(store, writer, config.Default().APITokens)
	teamScope := workspace.Scope{ID: team, TeamID: team}
	ownerToken, ownerSecret, err := tokens.Create(t.Context(), owner, teamScope, apitokens.CreateInput{Name: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	memberToken, secret, err := tokens.Create(t.Context(), member, teamScope, apitokens.CreateInput{Name: "ci", Scope: json.RawMessage(`[{"dst":["tag:ci"],"allow":["exec"]}]`)})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewRouter(Options{Config: config.Default(), Auth: auth.NewService(store, writer, nil, []string{"test-api-token-key"}, false),
		Workspaces: workspace.New(store), APITokens: tokens, Favorites: favorites.NewService(store), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	do := func(method, path, bearer, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code := func(out map[string]any) string {
		e, _ := out["error"].(map[string]any)
		s, _ := e["code"].(string)
		return s
	}

	status, out := do(http.MethodGet, "/v1/workspaces/"+team+"/api-tokens", secret, "")
	if status != http.StatusOK {
		t.Fatalf("list = %d %v", status, out)
	}
	list, _ := out["api_tokens"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != memberToken.ID || list[0].(map[string]any)["created_by"] != "user:"+member+"@example.test" || list[0].(map[string]any)["scope"] == nil {
		t.Fatalf("member sees %v", list)
	}
	if status, out = do(http.MethodGet, "/v1/workspaces/personal/api-tokens", secret, ""); status != http.StatusForbidden || code(out) != "workspace_unavailable" {
		t.Fatalf("other workspace path = %d %v", status, out)
	}
	if status, out = do(http.MethodGet, "/v1/favorites?workspace=personal", secret, ""); status != http.StatusForbidden || code(out) != "workspace_unavailable" {
		t.Fatalf("other workspace selector = %d %v", status, out)
	}
	if status, out = do(http.MethodGet, "/v1/favorites", ownerSecret, ""); status != http.StatusOK {
		t.Fatalf("an unscoped token acts as its creator in its workspace = %d %v", status, out)
	}
	// This token has a scope; until per-machine scope checks exist (F18-06) a scoped token is
	// refused outside the policy and API-token routes rather than widened.
	if status, out = do(http.MethodGet, "/v1/favorites", secret, ""); status != http.StatusForbidden || code(out) != "outside_token_scope" {
		t.Fatalf("scoped token outside its routes = %d %v", status, out)
	}
	if status, out = do(http.MethodPost, "/v1/workspaces/"+team+"/api-tokens", secret, `{"name":"more"}`); status != http.StatusForbidden || code(out) != "forbidden" {
		t.Fatalf("token creates token = %d %v", status, out)
	}
	if status, out = do(http.MethodDelete, "/v1/workspaces/"+team+"/api-tokens/"+ownerToken.ID, secret, ""); status != http.StatusNotFound {
		t.Fatalf("member revokes owner's token = %d %v", status, out)
	}
	if status, out = do(http.MethodDelete, "/v1/workspaces/"+team+"/api-tokens/"+memberToken.ID, secret, ""); status != http.StatusOK || out["api_token"].(map[string]any)["revoked_at"] == nil {
		t.Fatalf("revoke own = %d %v", status, out)
	}
	var metadata []byte
	if err := store.SQL().QueryRowContext(t.Context(), `SELECT metadata FROM audit_events WHERE resource_id=$1 AND event_type='api_token.revoked'`, memberToken.ID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metadata), `"api_token_id": "`+memberToken.ID+`"`) && !strings.Contains(string(metadata), `"api_token_id":"`+memberToken.ID+`"`) {
		t.Fatalf("audit metadata %s does not name the token", metadata)
	}
	if status, out = do(http.MethodGet, "/v1/favorites", secret, ""); status != http.StatusUnauthorized || code(out) != "api_token_invalid" {
		t.Fatalf("revoked = %d %v", status, out)
	}
	if status, out = do(http.MethodGet, "/v1/favorites", "pbat_garbage", ""); status != http.StatusUnauthorized || code(out) != "api_token_invalid" {
		t.Fatalf("malformed = %d %v", status, out)
	}
}

func TestAPITokenCreateValidationErrors(t *testing.T) {
	// The handler maps service errors without a database; the request fails before storage.
	tokens := apitokens.New(nil, nil, config.Default().APITokens)
	for body, want := range map[string]string{
		`{"name":"Bad Name"}`:                 "validation_failed",
		`{"name":"ok","expires_in_days":0}`:   "validation_failed",
		`{"name":"ok","expires_in_days":366}`: "validation_failed",
		`{"name":"ok","scope":[]}`:            "invalid_api_token_scope",
		`{"name":"ok","unknown":1}`:           "validation_failed",
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/personal/api-tokens", strings.NewReader(body))
		req.SetPathValue("ws", "personal")
		req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "user_1"}}))
		rec := httptest.NewRecorder()
		apiTokenCreate(tokens).ServeHTTP(rec, req)
		var out ErrorResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Error.Code != want {
			t.Fatalf("%s: %d %s, want %s", body, rec.Code, rec.Body, want)
		}
		if want == "invalid_api_token_scope" && (rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"errors"`)) {
			t.Fatalf("scope error: %d %s", rec.Code, rec.Body)
		}
	}
}
