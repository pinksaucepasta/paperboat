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

// Uses real session/machine authentication and assignment authority; only the
// external identity and repository provider are substituted.
func TestConfigNativeRepositoryAndConsentAuthorization(t *testing.T) {
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
	accounts := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"config-native-session"}, true, "https://login.pprbt.dev")
	cliAuth := auth.NewDeviceService(store, writer, cfg.CLIAuth, []string{"config-native-machine"})
	cliAuth.ConfigureAuthorizationEncryptionKey("test-device-recovery-key")
	assignments := controlplane.NewConfigAssignmentService(store, writer, "native-warning")
	router := authTestOriginHandler{next: NewRouter(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: accounts, DeviceAuth: cliAuth, ConfigAssignments: assignments, ConfigRepositoryAccess: controlplane.NewConfigRepositoryAccessService(store, nil, controlplane.ConfigRepositoryAccessIssuerFuncs{Issue: func(context.Context, string, string, string) (controlplane.ScopedRepositoryCredential, error) {
		return controlplane.ScopedRepositoryCredential{Token: "private-provider-fixture", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}, "", writer)})}
	suffix := fmt.Sprint(time.Now().UnixNano())
	ownerEmail, otherEmail := "config-owner-"+suffix+"@example.test", "config-other-"+suffix+"@example.test"
	ownerCookies := loginCookies(t, router, "workos_config_owner_"+suffix+":"+ownerEmail+":Owner")
	_ = loginCookies(t, router, "workos_config_other_"+suffix+":"+otherEmail+":Other")
	owner, other := userIDByEmail(t, store, ownerEmail), userIDByEmail(t, store, otherEmail)
	token := func(user string, scopes []string) string {
		t.Helper()
		grant, e := cliAuth.Authorize(ctx, auth.DeviceAuthorizationInput{ClientID: cfg.CLIAuth.ClientID, ClientLabel: "Config native", DeviceType: "desktop", OS: "linux", Scopes: cfg.CLIAuth.AllowedScopes, Network: "127.0.0.1"})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = cliAuth.Decide(ctx, grant.UserCode, user, true); e != nil {
			t.Fatal(e)
		}
		result, e := cliAuth.Poll(ctx, auth.DeviceTokenInput{ClientID: cfg.CLIAuth.ClientID, DeviceCode: grant.DeviceCode, Network: "127.0.0.1"})
		if e != nil {
			t.Fatal(e)
		}
		if len(scopes) == 1 {
			if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.cli_client_sessions SET scopes=ARRAY['machines:read']::text[] WHERE id=$1`, result.CLIClientSessionID); err != nil {
				t.Fatal(err)
			}
		}
		return result.AccessToken
	}
	ownerToken, otherToken, readToken := token(owner, cfg.CLIAuth.AllowedScopes), token(other, cfg.CLIAuth.AllowedScopes), token(owner, []string{"machines:read"})
	assignments.SetRepositoryResolver(controlplane.ConfigRepositoryResolverFunc(func(_ context.Context, user, provider, id string) (controlplane.ConfigRepositoryConnection, error) {
		if user != owner || provider != "github" || id != "12345" {
			return controlplane.ConfigRepositoryConnection{}, controlplane.ErrAssignmentForbidden
		}
		return controlplane.ConfigRepositoryConnection{ProviderAccountID: "provider-owner", ExternalRepositoryID: id, DisplayName: "owner/config", CloneURL: "https://github.com/owner/config.git", PublishURL: "https://github.com/owner/config.git", DefaultBranch: "main", AuthorizationRef: "github-user:" + user, CredentialCapability: "provider_user_repository"}, nil
	}))
	call := func(bearer string, cookies []*http.Cookie, path string, body any, want int) []byte {
		t.Helper()
		raw, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		addCookies(req, cookies)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s status=%d want=%d", path, rec.Code, want)
		}
		return rec.Body.Bytes()
	}
	repoPath := "/v1/config-repositories"
	repoBody := map[string]any{"provider": "github", "external_ref": "12345", "display_name": "untrusted-name"}
	call(readToken, nil, repoPath, repoBody, http.StatusForbidden)
	call("", ownerCookies, repoPath, repoBody, http.StatusForbidden)
	call(otherToken, nil, repoPath, repoBody, http.StatusBadRequest)
	var connected struct {
		Data struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(call(ownerToken, nil, repoPath, repoBody, http.StatusCreated), &connected); err != nil {
		t.Fatal(err)
	}
	if connected.Data.ID == "" || connected.Data.DisplayName != "owner/config" {
		t.Fatal("provider authority was not preserved")
	}
	machine, environment := "config_native_machine_"+suffix, "config_native_env_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$2,$3)`, environment, "config-native", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'Config host','linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, environment); err != nil {
		t.Fatal(err)
	}
	assignment, err := assignments.ConfigureProjection(ctx, owner, machine, connected.Data.ID, "", "pull_only", false, "native-warning", 0, nil, nil, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/machines/" + machine + "/config-assignment/consent"
	body := map[string]any{"warning_revision": "native-warning", "expected_version": assignment.Version}
	call(readToken, nil, path, body, http.StatusForbidden)
	call("", ownerCookies, path, body, http.StatusForbidden)
	call(otherToken, nil, path, body, http.StatusBadRequest)
	call(ownerToken, nil, path, map[string]any{"warning_revision": "wrong-warning", "expected_version": assignment.Version}, http.StatusConflict)
	call(ownerToken, nil, path, body, http.StatusOK)
	call(ownerToken, nil, path, body, http.StatusConflict)
	current, err := assignments.Assignment(ctx, owner, machine)
	if err != nil || current.ConsentState != "accepted" {
		t.Fatal("native consent did not persist")
	}

	customBody := map[string]any{"provider": "git", "external_ref": "ssh://git@private-lan/config.git", "display_name": "LAN config", "branch": "settings"}
	call(readToken, nil, repoPath, customBody, http.StatusForbidden)
	call(ownerToken, nil, repoPath, map[string]any{"provider": "git", "external_ref": "https://secret@private-lan/config.git", "display_name": "Unsafe"}, http.StatusBadRequest)
	var custom struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(call(ownerToken, nil, repoPath, customBody, http.StatusCreated), &custom); err != nil || custom.Data.ID == "" {
		t.Fatal("custom connect failed", err)
	}

	bootstrapPath := "/v1/config-repositories/" + custom.Data.ID + "/read-access"
	call("", nil, bootstrapPath, map[string]any{}, http.StatusUnauthorized)
	call("", ownerCookies, bootstrapPath, map[string]any{}, http.StatusUnauthorized)
	call(otherToken, nil, bootstrapPath, map[string]any{}, http.StatusBadRequest)
	readBody := call(readToken, nil, bootstrapPath, map[string]any{"url": "/srv/explicit-machine/config.git"}, http.StatusOK)
	var readGrant struct {
		Data controlplane.ConfigRepositoryReadAccess `json:"data"`
	}
	if err := json.Unmarshal(readBody, &readGrant); err != nil || readGrant.Data.RepositoryID != custom.Data.ID || readGrant.Data.Transport != "local" || readGrant.Data.Password != "" || readGrant.Data.Capability != "repository_contents_read" {
		t.Fatal("native bootstrap read grant invalid")
	}
	assignPath := "/v1/machines/" + machine + "/config-assignment"
	assignBody := map[string]any{"pull_repository_id": custom.Data.ID, "mode": "pull_only", "warning_revision": "native-warning", "expected_version": current.Version, "configuration_revision": strings.Repeat("b", 64), "path_rules": []map[string]any{{"id": "settings", "source": "machine", "repository_path": "editor/settings.json", "local_path": "/home/owner/editor/settings.json", "kind": "file", "include": []string{}, "exclude": []string{}}}, "repository_bindings": []map[string]string{{"repository_id": custom.Data.ID, "url": "/srv/shared/config.git"}}}
	put := func(bearer string, want int) []byte {
		t.Helper()
		raw, _ := json.Marshal(assignBody)
		req := httptest.NewRequest(http.MethodPut, assignPath, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("custom rules PUT status=%d want=%d", rec.Code, want)
		}
		return rec.Body.Bytes()
	}
	put(readToken, http.StatusForbidden)
	put(otherToken, http.StatusNotFound)
	var configured struct {
		Data configAssignmentResponse `json:"data"`
	}
	if err := json.Unmarshal(put(ownerToken, http.StatusOK), &configured); err != nil || len(configured.Data.PathRules) != 1 || len(configured.Data.RepositoryBindings) != 1 || configured.Data.PathRules[0].LocalPath != "/home/owner/editor/settings.json" {
		t.Fatal("explicit rules lost across HTTP", err)
	}
	put(ownerToken, http.StatusConflict)
}
