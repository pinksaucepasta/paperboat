package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/operator"
	"github.com/pinksaucepasta/paperboat/server/internal/operatoractions"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorinfra"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorreporting"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

func TestOperatorAuthenticationDatabaseOutageReachesFinalHTTPReporter(t *testing.T) {
	store, err := db.Open(config.Database{
		Driver: "postgres",
		DSN:    "postgres://paperboat:paperboat@127.0.0.1:1/operator_auth_test?sslmode=disable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	service := &operatorauth.Service{Store: store}
	opts := Options{OperatorAuth: service}
	mux := http.NewServeMux()
	registerOperatorAuthentication(mux, opts)
	mux.Handle("GET /v1/operator/protected", requireOperator(opts, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	invalidRequest, invalidLogs, _ := diagnosticRequest("/v1/operator/me")
	invalidResponse := httptest.NewRecorder()
	mux.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid session status = %d, want 401: %s", invalidResponse.Code, invalidResponse.Body.String())
	}
	if invalidLogs.Len() != 0 {
		t.Fatalf("expected credential rejection emitted an operational event: %s", invalidLogs.String())
	}

	const sessionCanary = "PRIVATE_SESSION_CANARY"
	cases := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/v1/operator/me"},
		{method: http.MethodPost, path: "/v1/operator/logout"},
		{method: http.MethodGet, path: "/v1/operator/protected"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			request, logs, reference := diagnosticRequest(tc.path)
			request.Method = tc.method
			request.AddCookie(&http.Cookie{Name: operatorSessionCookie, Value: sessionCanary})
			if tc.path == "/v1/operator/logout" {
				request.Header.Set("X-CSRF-Token", "PRIVATE_CSRF_CANARY")
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), sessionCanary) || strings.Contains(response.Body.String(), "database is closed") {
				t.Fatalf("public error exposed private or driver text: %s", response.Body.String())
			}

			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("expected exactly one structured fault event, got %q: %v", logs.String(), err)
			}
			if event["code"] != "operator_unavailable" || event["stage"] != "http_handler" || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" {
				t.Fatalf("fault event lacks the final owner identity: %#v", event)
			}
			if status, ok := event["http_status"].(float64); !ok || status != http.StatusServiceUnavailable {
				t.Fatalf("fault event status = %#v, want 503", event["http_status"])
			}
			if event["error_type"] == "" || strings.Contains(logs.String(), sessionCanary) || strings.Contains(logs.String(), "database is closed") {
				t.Fatalf("fault event lost its cause or exposed private text: %s", logs.String())
			}
		})
	}
}

func TestOperatorInfrastructureErrorRequiresPureDomainCause(t *testing.T) {
	pureCases := []struct {
		err    error
		status int
	}{
		{err: operatorinfra.ErrInvalid, status: http.StatusBadRequest},
		{err: operatorinfra.ErrForbidden, status: http.StatusForbidden},
		{err: operatorinfra.ErrNotFound, status: http.StatusNotFound},
		{err: operatorinfra.ErrConflict, status: http.StatusConflict},
	}
	for _, tc := range pureCases {
		request, logs, _ := diagnosticRequest("/v1/admin/nodes/node_test/update")
		response := httptest.NewRecorder()
		operatorInfrastructureError(response, request, tc.err)
		if response.Code != tc.status {
			t.Errorf("pure %v status = %d, want %d", tc.err, response.Code, tc.status)
		}
		if logs.Len() != 0 {
			t.Errorf("pure expected response logged as an operational fault: %s", logs.String())
		}
	}

	request, logs, reference := diagnosticRequest("/v1/admin/nodes/node_test/update")
	response := httptest.NewRecorder()
	operatorInfrastructureError(response, request, errors.Join(operatorinfra.ErrConflict, syscall.EIO))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("mixed failure status = %d, want 503: %s", response.Code, response.Body.String())
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one structured fault event, got %q: %v", logs.String(), err)
	}
	if event["code"] != "infrastructure_unavailable" || event["stage"] != "http_handler" || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" {
		t.Fatalf("fault event lacks the final owner identity: %#v", event)
	}
	if errno, ok := event["errno"].(float64); !ok || errno != float64(syscall.EIO) {
		t.Fatalf("fault event errno = %#v, want EIO", event["errno"])
	}
}

func TestOperatorInfrastructureClosedStoreReachesFinalReporter(t *testing.T) {
	store, err := db.Open(config.Database{
		Driver: "postgres",
		DSN:    "postgres://paperboat:paperboat@127.0.0.1:1/operator_infra_test?sslmode=disable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service := operatorinfra.NewService(store, "local-infrastructure-test-key")
	_, cause := service.ListRegions(context.Background(), "operator_test", "", 10)
	if cause == nil {
		t.Fatal("closed store unexpectedly completed infrastructure listing")
	}

	request, logs, reference := diagnosticRequest("/v1/admin/regions")
	response := httptest.NewRecorder()
	operatorInfrastructureError(response, request, cause)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed store status = %d, want 503: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "database is closed") {
		t.Fatalf("public infrastructure response exposed driver text: %s", response.Body.String())
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected exactly one structured fault event, got %q: %v", logs.String(), err)
	}
	if event["code"] != "infrastructure_unavailable" || event["stage"] != "http_handler" || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" || event["error_type"] == "" {
		t.Fatalf("closed store event lost failure identity or cause: %#v", event)
	}
}

func TestOperatorConnectedAuthenticationDisclosureAndRevocation(t *testing.T) {
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
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	subject := "workos_" + supportreport.NewReference()
	claims, _ := json.Marshal(map[string]any{"sid": "session_" + strconv.FormatInt(time.Now().UnixNano(), 10), "sub": subject, "iss": "https://api.workos.com/", "exp": time.Now().Add(time.Hour).Unix()})
	accessToken := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]string
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Error("bad provider request")
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"id": subject, "email": "staff@example.test", "email_verified": true}, "authentication_method": "Passkey", "access_token": accessToken, "refresh_token": "refresh_test"})
	}))
	defer provider.Close()
	service := auth.NewService(store, audit.NewWriter(store), auth.HTTPWorkOSVerifier{BaseURL: provider.URL, ClientID: "client_test", APIKey: "not-a-real-secret", HTTPClient: provider.Client()}, []string{"test-cookie-key"}, false, "http://localhost:3000")
	user, session, err := service.VerifyCallback(ctx, auth.CallbackInput{Code: "login"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.Pool().Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id=$1`, user.ID)
		store.Pool().Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, user.ID)
		store.Pool().Exec(ctx, `DELETE FROM user_identities WHERE user_id=$1`, user.ID)
		store.Pool().Exec(ctx, `DELETE FROM users WHERE id=$1`, user.ID)
	}()
	if _, err = store.Pool().Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	operatorEmail := "operator-" + supportreport.NewReference() + "@example.test"
	operatorService := &operatorauth.Service{Store: store, BootstrapEmail: operatorEmail}
	operatorPassword := "operator test password for connected flow"
	var operatorID, operatorCSRF string
	defer func() {
		if operatorID == "" {
			return
		}
		store.Pool().Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id=$1`, operatorID)
		store.Pool().Exec(ctx, `DELETE FROM operator_bootstrap WHERE operator_id=$1`, operatorID)
		store.Pool().Exec(ctx, `DELETE FROM operator_accounts WHERE id=$1`, operatorID)
	}()
	cookies := map[string]*http.Cookie{}
	record := httptest.NewRecorder()
	service.SetSessionCookies(record, session)
	for _, cookie := range record.Result().Cookies() {
		cookies[cookie.Name] = cookie
	}
	cfg := config.Default()
	cfg.HTTP.AllowedOrigins = []string{"http://localhost:3000"}
	cfg.HTTP.BrowserLoginOrigin = "http://localhost:3000"
	backupToken := strings.Repeat("backup-test-only", 3)
	backupService, err := backupreport.NewService(store, backupreport.Config{Token: backupToken, DatabaseID: "http-" + user.ID})
	if err != nil {
		t.Fatal(err)
	}
	reportingService, err := operatorreporting.New(store, audit.NewWriter(store), operatorreporting.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Pool().Exec(ctx, `DELETE FROM backup_reports WHERE database_id=$1`, "http-"+user.ID)
	infrastructureService := operatorinfra.NewService(store, strings.Repeat("infra-test-key", 3))
	router := NewRouter(Options{OperatorInfrastructure: infrastructureService, OperatorBackups: backupService, OperatorReporting: reportingService, Config: cfg, Auth: service, OperatorAuth: operatorService, Operator: operator.NewService(store), OperatorActions: operatoractions.NewService(store), OperatorAudit: audit.NewWriter(store), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := func(method, path, body string, csrf bool, extra ...map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", "http://localhost:3000")
		r.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		if csrf {
			r.Header.Set("X-CSRF-Token", operatorCSRF)
		}
		for _, headers := range extra {
			for key, value := range headers {
				r.Header.Set(key, value)
			}
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		for _, cookie := range response.Result().Cookies() {
			cookies[cookie.Name] = cookie
		}
		return response
	}
	path := "/v1/admin/accounts/" + user.ID
	if r := request("GET", path, "", false); r.Code != 403 {
		t.Fatalf("ordinary login elevated: %d %s", r.Code, r.Body.String())
	}
	credentials, _ := json.Marshal(map[string]string{"email": operatorEmail, "password": operatorPassword})
	if r := request("POST", "/v1/operator/setup", string(credentials), false); r.Code != 200 {
		t.Fatalf("operator setup: %d %s", r.Code, r.Body.String())
	} else {
		var result struct {
			Data struct {
				CSRFToken string `json:"csrf_token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || result.Data.CSRFToken == "" {
			t.Fatalf("operator setup response: %v", err)
		}
		operatorCSRF = result.Data.CSRFToken
	}
	account, err := operatorService.Authenticate(ctx, cookies[operatorSessionCookie].Value, operatorCSRF, true)
	if err != nil {
		t.Fatal(err)
	}
	operatorID = account.ID
	if r := request("POST", "/v1/operator/setup", string(credentials), false); r.Code != 409 {
		t.Fatalf("bootstrap reused: %d", r.Code)
	}
	if r := request("POST", "/v1/operator/logout", "{}", false); r.Code != 403 {
		t.Fatalf("operator logout accepted without CSRF: %d", r.Code)
	}
	if r := request("GET", path, "", false); r.Code != 200 || !strings.Contains(r.Body.String(), "staff@example.test") {
		t.Fatalf("audited read: %d %s", r.Code, r.Body.String())
	}

	for _, path := range []string{"/v1/admin/backups", "/v1/admin/backups/runs", "/v1/admin/support"} {
		if response := request("GET", path, "", false); response.Code != 200 {
			t.Fatalf("connected operator evidence %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	if response := request("GET", "/v1/admin/sentry/issues", "", false); response.Code != 503 || !strings.Contains(response.Body.String(), "sentry_not_configured") {
		t.Fatalf("Sentry disabled state %d", response.Code)
	}
	report := backupreport.Report{DatabaseID: "http-" + user.ID, RunID: "run-" + user.ID, Kind: "backup", StartedAt: time.Now().Add(-time.Minute), CompletedAt: time.Now().Add(-time.Second), Outcome: "succeeded", Bytes: 100, SHA256: strings.Repeat("a", 64), RetainedUntil: time.Now().Add(24 * time.Hour)}
	reportBody, _ := json.Marshal(report)
	if response := request("POST", "/v1/operations/backups/reports", string(reportBody), true); response.Code != 401 {
		t.Fatalf("browser cannot mint backup evidence: %d", response.Code)
	}
	machineRequest := httptest.NewRequest("POST", "/v1/operations/backups/reports", strings.NewReader(string(reportBody)))
	machineRequest.Header.Set("Authorization", "Bearer "+backupToken)
	machineRequest.Header.Set("Content-Type", "application/json")
	machineResponse := httptest.NewRecorder()
	router.ServeHTTP(machineResponse, machineRequest)
	if machineResponse.Code != 200 {
		t.Fatalf("producer report %d %s", machineResponse.Code, machineResponse.Body.String())
	}
	if response := request("GET", "/v1/admin/backups", "", false); response.Code != 200 || !strings.Contains(response.Body.String(), `"backup_state":"current"`) || !strings.Contains(response.Body.String(), `"restore_state":"unknown"`) {
		t.Fatalf("backup is not restore: %d %s", response.Code, response.Body.String())
	}

	regionID := "r" + strings.ReplaceAll(user.ID, "_", "-")
	nodeID := "node_" + strings.ReplaceAll(user.ID, "-", "_")
	defer func() {
		store.Pool().Exec(ctx, `DELETE FROM operator_infrastructure_operations WHERE actor_user_id=$1`, operatorID)
		store.Pool().Exec(ctx, `DELETE FROM managed_node_authority WHERE node_id=$1`, nodeID)
		store.Pool().Exec(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, nodeID)
		store.Pool().Exec(ctx, `DELETE FROM operator_regions WHERE id=$1`, regionID)
	}()
	regionPath := "/v1/admin/regions/" + regionID + "/create"
	if response := request("POST", regionPath, `{"revision":0,"display_name":"Test region"}`, true); response.Code != 400 {
		t.Fatalf("unconfirmed region creation %d", response.Code)
	}
	infraHeaders := map[string]string{"Operator-Target": regionID, "Operator-Reason": "http-case", "Idempotency-Key": "region-" + user.ID}
	if response := request("POST", regionPath, `{"revision":0,"display_name":"Test region"}`, true, infraHeaders); response.Code != 200 {
		t.Fatalf("region creation %d %s", response.Code, response.Body.String())
	}
	nodeInput := operatorNodeInput{Revision: 0, RegionID: regionID, EdgePool: regionID, FailureDomain: "rack-a", EndpointHost: "relay.example.test", EndpointTCPPort: 443, EndpointQUICPort: 444, Roles: []string{"relay"}, Transports: []string{"derp_quic", "derp_wss"}, CapacityLimit: 32}
	nodeBody, _ := json.Marshal(nodeInput)
	infraHeaders["Operator-Target"] = nodeID
	infraHeaders["Idempotency-Key"] = "node-enroll-" + user.ID
	nodeResponse := request("POST", "/v1/admin/nodes/"+nodeID+"/enroll", string(nodeBody), true, infraHeaders)
	if nodeResponse.Code != 200 {
		t.Fatalf("node enrollment %d %s", nodeResponse.Code, nodeResponse.Body.String())
	}
	var enrolled struct {
		Data operatorinfra.NodeMutationResult `json:"data"`
	}
	if err = json.Unmarshal(nodeResponse.Body.Bytes(), &enrolled); err != nil || enrolled.Data.RuntimeCredential == "" || enrolled.Data.Node.Ready {
		t.Fatal("enrollment authority", err)
	}
	if resolved, err := infrastructureService.RuntimeNode(ctx, enrolled.Data.RuntimeCredential); err != nil || resolved != nodeID {
		t.Fatal("scoped runtime credential", err)
	}
	if listed := request("GET", "/v1/admin/nodes", "", false); listed.Code != 200 || strings.Contains(listed.Body.String(), enrolled.Data.RuntimeCredential) {
		t.Fatal("node list disclosed credential", listed.Code)
	}
	if replay := request("POST", "/v1/admin/nodes/"+nodeID+"/enroll", string(nodeBody), true, infraHeaders); replay.Code != 200 || replay.Body.String() != nodeResponse.Body.String() {
		t.Fatal("enrollment replay changed", replay.Code)
	}
	infraHeaders["Operator-Target"] = regionID
	infraHeaders["Idempotency-Key"] = "region-disable-" + user.ID
	if response := request("POST", "/v1/admin/regions/"+regionID+"/disable", `{"revision":1}`, true, infraHeaders); response.Code != 200 {
		t.Fatalf("disable region %d %s", response.Code, response.Body.String())
	}
	if _, err := infrastructureService.RuntimeNode(ctx, enrolled.Data.RuntimeCredential); err == nil {
		t.Fatal("disabled region runtime authority accepted")
	}

	target := user.ID + "_target"
	if _, err = store.Pool().Exec(ctx, `INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, target, target+"@example.test"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.Pool().Exec(ctx, `DELETE FROM operator_actions WHERE actor_user_id=$1`, operatorID)
		store.Pool().Exec(ctx, `DELETE FROM users WHERE id=$1`, target)
	}()
	banPath := "/v1/admin/accounts/" + target + "/ban"
	if r := request("POST", banPath, `{"revision":1}`, true); r.Code != 400 {
		t.Fatalf("unconfirmed ban accepted: %d %s", r.Code, r.Body.String())
	}
	headers := map[string]string{"Operator-Target": target, "Operator-Reason": "case_http", "Idempotency-Key": "http-ban-" + target}
	ban := request("POST", banPath, `{"revision":1}`, true, headers)
	if ban.Code != 200 {
		t.Fatalf("connected ban: %d %s", ban.Code, ban.Body.String())
	}
	if replay := request("POST", banPath, `{"revision":1}`, true, headers); replay.Code != 200 || replay.Body.String() != ban.Body.String() {
		t.Fatalf("connected replay: %d %s", replay.Code, replay.Body.String())
	}
	var state string
	if err = store.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, target).Scan(&state); err != nil || state != "suspended" {
		t.Fatal("ban not persisted", state, err)
	}
	headers["Idempotency-Key"] = "http-unban-" + target
	if r := request("POST", "/v1/admin/accounts/"+target+"/unban", `{"revision":2}`, true, headers); r.Code != 200 {
		t.Fatalf("connected unban: %d %s", r.Code, r.Body.String())
	}
	var n int
	if err = store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND event_type='operator.read'`, operatorID).Scan(&n); err != nil || n != 1 {
		t.Fatal("read audit missing", err, n)
	}
	if _, err = store.Pool().Exec(ctx, `UPDATE operator_sessions SET expires_at=now()-interval '1 second' WHERE operator_id=$1`, operatorID); err != nil {
		t.Fatal(err)
	}
	if r := request("GET", path, "", false); r.Code != 403 || strings.Contains(r.Body.String(), "staff@example.test") {
		t.Fatal("expired operator session disclosed data")
	}
	badCredentials, _ := json.Marshal(map[string]string{"email": operatorEmail, "password": "wrong operator password"})
	if r := request("POST", "/v1/operator/login", string(badCredentials), false); r.Code != 401 {
		t.Fatalf("bad password accepted: %d", r.Code)
	}
	if r := request("POST", "/v1/operator/login", string(credentials), false); r.Code != 200 {
		t.Fatalf("operator login: %d %s", r.Code, r.Body.String())
	} else {
		var result struct {
			Data struct {
				CSRFToken string `json:"csrf_token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || result.Data.CSRFToken == "" {
			t.Fatalf("operator login response: %v", err)
		}
		operatorCSRF = result.Data.CSRFToken
	}
	if r := request("GET", path, "", false); r.Code != 200 {
		t.Fatalf("operator login did not restore access: %d %s", r.Code, r.Body.String())
	}
	if _, err = store.Pool().Exec(ctx, `UPDATE operator_accounts SET role='operations' WHERE id=$1`, operatorID); err != nil {
		t.Fatal(err)
	}
	if r := request("GET", path, "", false); r.Code != 403 {
		t.Fatal("role change did not revoke account-read authority")
	}
	if _, err = store.Pool().Exec(ctx, `UPDATE operator_accounts SET role='admin' WHERE id=$1`, operatorID); err != nil {
		t.Fatal(err)
	}
	if r := request("GET", path, "", false); r.Code != 200 {
		t.Fatal("restored role was not honored")
	}
	if _, err = store.Pool().Exec(ctx, `UPDATE operator_accounts SET status='disabled' WHERE id=$1`, operatorID); err != nil {
		t.Fatal(err)
	}
	if r := request("GET", path, "", false); r.Code != 403 {
		t.Fatal("operator account revocation ignored")
	}
	if _, err = store.Pool().Exec(ctx, `UPDATE operator_accounts SET status='active' WHERE id=$1`, operatorID); err != nil {
		t.Fatal(err)
	}
	if r := request("POST", "/v1/operator/logout", "{}", true); r.Code != 200 {
		t.Fatalf("operator logout: %d %s", r.Code, r.Body.String())
	}
	if r := request("GET", path, "", false); r.Code != 403 {
		t.Fatal("logged-out operator session retained access")
	}
}
