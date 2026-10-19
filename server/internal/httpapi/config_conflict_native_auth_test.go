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
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/testauth"
)

func TestConfigNativeConflictResolutionAuthorization(t *testing.T) {
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
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	writer := audit.NewWriter(store)
	accounts := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"conflict-native-session"}, true, "https://login.example.test")
	cliAuth := auth.NewDeviceService(store, writer, cfg.CLIAuth, []string{"conflict-native-machine"})
	cliAuth.ConfigureAuthorizationEncryptionKey("test-device-recovery-key")
	conflicts := controlplane.NewConfigConflictService(store, nil, writer)
	// Request calls use the raw router: never inject an Origin into native requests.
	router := NewRouter(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: accounts, DeviceAuth: cliAuth, ConfigConflicts: conflicts})
	suffix := fmt.Sprint(time.Now().UnixNano())
	email := "conflict-owner-" + suffix + "@example.test"
	cookies := loginCookies(t, authTestOriginHandler{next: router}, "workos_conflict_owner_"+suffix+":"+email+":Owner")
	otherEmail := "conflict-other-" + suffix + "@example.test"
	_ = loginCookies(t, authTestOriginHandler{next: router}, "workos_conflict_other_"+suffix+":"+otherEmail+":Other")
	owner, other := userIDByEmail(t, store, email), userIDByEmail(t, store, otherEmail)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1 OR id=$2`, owner, other)
	})
	token := func(user string) (string, string) {
		t.Helper()
		grant, err := cliAuth.Authorize(ctx, auth.DeviceAuthorizationInput{ClientID: cfg.CLIAuth.ClientID, ClientLabel: "Conflict test", DeviceType: "desktop", OS: "linux", Scopes: cfg.CLIAuth.AllowedScopes, Network: "127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cliAuth.Decide(ctx, grant.UserCode, user, true, ""); err != nil {
			t.Fatal(err)
		}
		result, err := cliAuth.Poll(ctx, auth.DeviceTokenInput{ClientID: cfg.CLIAuth.ClientID, DeviceCode: grant.DeviceCode, Network: "127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		return result.AccessToken, result.CLIClientSessionID
	}
	ownerToken, clientID := token(owner)
	otherToken, _ := token(other)
	environment, machine, repository, assignment := "conflict_env_"+suffix, "conflict_machine_"+suffix, "conflict_repo_"+suffix, "conflict_assignment_"+suffix
	revision := strings.Repeat("a", 64)
	fixtures := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, []any{environment, owner}},
		{`INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root) VALUES($1,$2,$3,'Machine','linux','amd64','/workspace')`, []any{machine, owner, environment}},
		{`INSERT INTO paperboat.control_config_repositories(id,owner_user_id,provider,external_ref,display_name) VALUES($1,$2,'git',$1,'Config')`, []any{repository, owner}},
		{`INSERT INTO paperboat.control_config_assignments(id,machine_id,environment_id,repository_id,mode,consent_state,warning_revision,accepted_at,configuration_revision,version) VALUES($1,$2,$3,$4,'bidirectional','accepted','warning-1',now(),repeat('a',64),3)`, []any{assignment, machine, environment, repository}},
		{`INSERT INTO paperboat.control_config_sync_statuses(environment_id,repository_id,assignment_id,machine_id,installation_generation,warning_revision,policy_revision,sync_revision,state,conflicts,remote_revision,machine_updated_at,observed_at) VALUES($1,$2,$3,$4,1,'warning-1','policy-1',2,'conflict',$5::jsonb,'head-2',now(),now())`, []any{environment, repository, assignment, machine, `[{"path":"myapp/settings.json","revision":"` + revision + `"}]`}},
	}
	for _, f := range fixtures {
		if _, err := store.SQL().ExecContext(ctx, f.query, f.args...); err != nil {
			t.Fatal(err)
		}
	}
	browserCSRF := ""
	call := func(path, bearer, origin string, sessionCookies []*http.Cookie, body any, want int, wantCode string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if browserCSRF != "" {
			request.Header.Set(auth.CSRFHeaderName, browserCSRF)
		}
		addCookies(request, sessionCookies)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Fatalf("%s status=%d want=%d", path, recorder.Code, want)
		}
		if wantCode != "" && !strings.Contains(recorder.Body.String(), `"code":"`+wantCode+`"`) {
			t.Fatalf("%s expected error code %s", path, wantCode)
		}
	}
	base := "/v1/config-sync/environments/" + environment
	request := map[string]any{"path": "myapp/settings.json", "conflict_revision": revision, "expected_remote_revision": "head-2", "expected_assignment_version": 3, "action": "keep_remote"}
	force := map[string]any{"scope": "config", "action": "force_pull", "expected_remote_revision": "head-2", "expected_assignment_version": 3, "confirmation": "FORCE PULL"}
	for _, route := range []struct {
		path      string
		body      any
		staleCode string
	}{{base + "/conflict-resolutions", request, "conflict_resolution_stale"}, {base + "/force", force, "force_request_stale"}} {
		call(route.path, "", "", cookies, route.body, http.StatusForbidden, "origin_failed")
		call(route.path, "", "https://evil.example", cookies, route.body, http.StatusForbidden, "origin_failed")
		call(route.path, "", "https://login.example.test", cookies, route.body, http.StatusForbidden, "csrf_failed")
		call(route.path, "invalid-token", "", cookies, route.body, http.StatusUnauthorized, "unauthenticated")
		call(route.path, otherToken, "", nil, route.body, http.StatusConflict, route.staleCode)
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.cli_client_sessions SET scopes=ARRAY['machines:read']::text[] WHERE id=$1`, clientID); err != nil {
			t.Fatal(err)
		}
		call(route.path, ownerToken, "", nil, route.body, http.StatusForbidden, "insufficient_scope")
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.cli_client_sessions SET scopes=ARRAY['machines:connect']::text[] WHERE id=$1`, clientID); err != nil {
			t.Fatal(err)
		}
		call(route.path, ownerToken, "", nil, route.body, http.StatusAccepted, "")
	}
	browserCSRF = csrfCookie(t, cookies)
	call(base+"/conflict-resolutions", "", "https://login.example.test", cookies, request, http.StatusAccepted, "")
	call(base+"/conflict-resolutions", "", "", cookies, request, http.StatusForbidden, "origin_failed")
	browserCSRF = ""
	request["expected_remote_revision"] = "stale-head"
	call(base+"/conflict-resolutions", ownerToken, "", nil, request, http.StatusConflict, "conflict_resolution_stale")
}
