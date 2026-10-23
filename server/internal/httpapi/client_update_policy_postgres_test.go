package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
)

func TestClientUpdatePolicyOperatorPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires fresh isolated remote PostgreSQL")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e = db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	var consumed bool
	if e = store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.operator_bootstrap)`).Scan(&consumed); e != nil {
		t.Fatal(e)
	}
	if consumed {
		t.Fatal("fresh operator bootstrap required; do not run this against a shared or deployed database")
	}
	email := "policy-" + uuid.NewString() + "@example.test"
	password := "policy integration fixture " + uuid.NewString()
	op := &operatorauth.Service{Store: store, BootstrapEmail: email}
	policy := clientupdate.New(store, audit.NewWriter(store), "")
	original, e := policy.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}
	var actor string
	defer func() {
		if actor == "" {
			return
		}
		_, err := store.Pool().Exec(ctx, `UPDATE paperboat.client_update_policy SET revision=$1,minimum_version=$2,reason=$3,enforce_at=$4 WHERE singleton`, original.Revision, original.MinimumVersion, original.Reason, original.EnforceAt)
		if err != nil {
			t.Error(err)
		}
		for _, q := range []string{`DELETE FROM paperboat.client_update_policy_requests WHERE actor_id=$1`} {
			if _, err = store.Pool().Exec(ctx, q, actor); err != nil {
				t.Error(err)
			}
		}
		// Retain immutable audits and their account/bootstrap references for
		// the test lead to remove by dropping this isolated database.
	}()
	opts := Options{OperatorAuth: op, OperatorAudit: audit.NewWriter(store), ClientUpdatePolicy: policy, Config: config.Config{HTTP: config.HTTPConfig{AllowedOrigins: []string{"https://console.example.test"}, RequestTimeout: 5 * time.Second, MaxBodyBytes: 8192}}}
	router := NewRouter(opts)
	request := func(method, path, body, token, csrf, origin, target, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.AddCookie(&http.Cookie{Name: operatorSessionCookie, Value: token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Operator-Target", target)
		r.Header.Set("Operator-Reason", "test_reference")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	origin := "https://console.example.test"
	credentials, _ := json.Marshal(map[string]string{"email": email, "password": password})
	setup := request("POST", "/v1/operator/setup", string(credentials), "", "", origin, "", "")
	if setup.Code != 200 {
		t.Fatalf("setup failed status=%d", setup.Code)
	}
	var payload struct {
		Data struct {
			CSRF string `json:"csrf_token"`
			User struct {
				ID string `json:"id"`
			} `json:"operator"`
		}
	}
	if e = json.Unmarshal(setup.Body.Bytes(), &payload); e != nil {
		t.Fatal(e)
	}
	// Read the real application-generated identity; no prefix fixture bypasses setup.
	if e = store.Pool().QueryRow(ctx, `SELECT id FROM paperboat.operator_accounts WHERE email=$1`, email).Scan(&actor); e != nil {
		t.Fatal(e)
	}
	if err := audit.NewWriter(store).Write(ctx, audit.Event{ActorUserID: "operator_" + uuid.NewString(), ActorType: audit.ActorAdmin, EventType: "client_update_policy.test_unknown_actor", ResourceType: "client_update_policy", ResourceID: "client-update-policy"}); err == nil {
		t.Fatal("unknown operator actor was accepted")
	}
	login := request("POST", "/v1/operator/login", string(credentials), "", "", origin, "", "")
	if login.Code != 200 {
		t.Fatalf("login status=%d", login.Code)
	}
	var token string
	for _, c := range login.Result().Cookies() {
		if c.Name == operatorSessionCookie {
			token = c.Value
		}
	}
	if e = json.Unmarshal(login.Body.Bytes(), &payload); e != nil {
		t.Fatal(e)
	}
	csrf := payload.Data.CSRF
	if token == "" || csrf == "" {
		t.Fatal("operator session or CSRF missing")
	}
	me := request("GET", "/v1/operator/me", "", token, "", origin, "", "")
	if me.Code != 200 {
		t.Fatalf("me status=%d", me.Code)
	}
	body := fmt.Sprintf(`{"expected_revision":%d,"minimum_version":"","reason":""}`, original.Revision)
	for _, tc := range []struct {
		name, role, origin, csrf, target string
		want                             int
	}{
		{"missing_csrf", "admin", origin, "", "client-update-policy", 403},
		{"wrong_origin", "admin", "https://untrusted.example.test", csrf, "client-update-policy", 403},
		{"support", "support", origin, csrf, "client-update-policy", 403},
		{"wrong_target", "admin", origin, csrf, "other", 400},
		{"admin", "admin", origin, csrf, "client-update-policy", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e = store.Pool().Exec(ctx, `UPDATE paperboat.operator_accounts SET role=$2 WHERE id=$1`, actor, tc.role); e != nil {
				t.Fatal(e)
			}
			w := request("PUT", "/v1/admin/client-update-policy", body, token, tc.csrf, tc.origin, tc.target, "test-policy-key")
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	replay := request("PUT", "/v1/admin/client-update-policy", body, token, csrf, origin, "client-update-policy", "test-policy-key")
	if replay.Code != 200 {
		t.Fatal("retry", replay.Code)
	}
	stale := request("PUT", "/v1/admin/client-update-policy", body, token, csrf, origin, "client-update-policy", "test-stale-key")
	if stale.Code != 409 {
		t.Fatal("stale", stale.Code)
	}
	current, e := policy.Get(ctx)
	if e != nil || current.Revision != original.Revision+1 {
		t.Fatal("unauthorized or duplicate mutation", e)
	}
	logout := request("POST", "/v1/operator/logout", "", token, csrf, origin, "", "")
	if logout.Code != 200 {
		t.Fatal("logout", logout.Code)
	}
	revoked := request("GET", "/v1/admin/client-update-policy", "", token, "", origin, "", "")
	if revoked.Code != 403 {
		t.Fatal("revoked", revoked.Code)
	}
}
