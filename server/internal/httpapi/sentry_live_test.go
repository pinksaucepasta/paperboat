package httpapi

// Opt-in connected acceptance: isolated PostgreSQL plus real Sentry. Identity
// provider exchange is a fixture; application authentication and audit are real.
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/operator"
	"github.com/pinksaucepasta/paperboat-server/internal/operatoractions"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorauth"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorinfra"
	"github.com/pinksaucepasta/paperboat-server/internal/operatorreporting"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestLiveSentryOperationalReadback(t *testing.T) {
	if os.Getenv("PAPERBOAT_LIVE_SENTRY_DSN") == "" || os.Getenv("PAPERBOAT_LIVE_SENTRY_TOKEN") == "" {
		t.Skip("requires explicit live Sentry credentials")
	}
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal("requires isolated test DB")
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal("test DB unavailable")
	}
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	sqlStore, err := supportreport.NewSQLStore(store)
	if err != nil {
		t.Fatal(err)
	}
	rate := 1.0
	reports, err := supportreport.New(sqlStore, supportreport.Config{Enabled: true, DSN: os.Getenv("PAPERBOAT_LIVE_SENTRY_DSN"), Release: "paperboat-server:validation-20260919", Environment: "sentry-validation", Region: "validation", Instance: "slot-01", TracesSampleRate: &rate})
	if err != nil {
		t.Fatal(err)
	}
	reports.SetSnapshot(func() { reports.Gauge("paperboat.runtime.goroutines", 42) })
	runctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = reports.Run(runctx) }()
	defer func() { cancel(); <-done; reports.Close() }()
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
	if _, err = store.Pool().Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	operatorEmail := "operator-" + supportreport.NewReference() + "@example.test"
	operatorService := &operatorauth.Service{Store: store, BootstrapEmail: operatorEmail}
	operatorPassword := "operator test password for Sentry readback"
	var operatorID, operatorCSRF string
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
	reportingService, err := operatorreporting.New(store, audit.NewWriter(store), operatorreporting.Config{BaseURL: "https://de.sentry.io", Organization: "pprbt", Projects: []string{"go"}, Token: os.Getenv("PAPERBOAT_LIVE_SENTRY_TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Pool().Exec(ctx, `DELETE FROM backup_reports WHERE database_id=$1`, "http-"+user.ID)
	infrastructureService := operatorinfra.NewService(store, strings.Repeat("infra-test-key", 3))
	router := NewRouter(Options{SupportReports: reports, OperatorInfrastructure: infrastructureService, OperatorBackups: backupService, OperatorReporting: reportingService, Config: cfg, Auth: service, OperatorAuth: operatorService, Operator: operator.NewService(store), OperatorActions: operatoractions.NewService(store), OperatorAudit: audit.NewWriter(store), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	if response := request("GET", "/v1/admin/sentry/monitors?project=go", "", false); response.Code != 403 {
		t.Fatal("ordinary login accepted")
	}
	credentials, _ := json.Marshal(map[string]string{"email": operatorEmail, "password": operatorPassword})
	setup := request("POST", "/v1/operator/setup", string(credentials), false)
	if setup.Code != 200 {
		t.Fatalf("operator setup HTTP %d %s", setup.Code, setup.Body.String())
	}
	var setupResult struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(setup.Body.Bytes(), &setupResult); err != nil || setupResult.Data.CSRFToken == "" {
		t.Fatalf("operator setup response: %v", err)
	}
	operatorCSRF = setupResult.Data.CSRFToken
	account, err := operatorService.Authenticate(ctx, cookies[operatorSessionCookie].Value, operatorCSRF, true)
	if err != nil {
		t.Fatal(err)
	}
	operatorID = account.ID
	ref := os.Getenv("PAPERBOAT_LIVE_SENTRY_REFERENCE")
	if !supportreport.ValidReference(ref) {
		ref = supportreport.NewReference()
	}
	failure := NewRouter(Options{Config: cfg, Auth: service, SupportReports: reports, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OverrideHandler: requireAuth(service, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, 500, "internal_error", "The controlled acceptance operation failed.")
	}))})
	r := httptest.NewRequest("GET", "/v1/support/acceptance", nil)
	r.Header.Set("Support-Reference", ref)
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	failed := httptest.NewRecorder()
	failure.ServeHTTP(failed, r)
	if failed.Code != 500 || failed.Header().Get("Support-Reference") != ref {
		t.Fatal("public reference lost")
	}
	for _, cookie := range failed.Result().Cookies() {
		cookies[cookie.Name] = cookie
	}
	for _, path := range []string{"/v1/admin/sentry/monitors?project=go", "/v1/admin/sentry/detectors?project=go", "/v1/admin/sentry/issues", "/v1/admin/sentry/metrics/catalog"} {
		response := request("GET", path, "", false)
		if response.Code != 200 {
			t.Fatalf("read %s HTTP %d", path, response.Code)
		}
		t.Logf("verified %s", path)
	}
	if monitor := os.Getenv("PAPERBOAT_LIVE_SENTRY_MONITOR"); monitor != "" {
		response := request("GET", "/v1/admin/sentry/monitors/"+url.PathEscape(monitor)+"/checkins?project=go", "", false)
		if response.Code != 200 {
			t.Fatalf("check-ins HTTP %d", response.Code)
		}
		var page struct {
			Data operatorreporting.CheckInPage `json:"data"`
		}
		if json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Data.CheckIns) == 0 {
			t.Fatal("missing check-ins")
		}
		t.Logf("verified %d check-ins", len(page.Data.CheckIns))
	}
	outcomes := request("GET", "/v1/admin/sentry/outcomes?project=go&period=24h", "", false)
	if outcomes.Code != 200 {
		t.Fatalf("ingestion outcomes HTTP %d", outcomes.Code)
	}
	var ingestion struct {
		Data operatorreporting.IngestionOutcomeSeries `json:"data"`
	}
	if json.Unmarshal(outcomes.Body.Bytes(), &ingestion) != nil {
		t.Fatal("invalid ingestion outcomes")
	}
	accepted := map[string]bool{}
	for _, outcome := range ingestion.Data.Outcomes {
		if outcome.Outcome == "accepted" && outcome.Quantity > 0 {
			accepted[outcome.Category] = true
		}
	}
	for _, category := range []string{"error", "log_item", "trace_metric", "span"} {
		if !accepted[category] {
			t.Fatalf("missing accepted ingestion category %s", category)
		}
	}
	t.Log("verified accepted errors, logs, metrics and spans in provider ingestion outcomes")
	// Stop the exporter before readback so short-lived process flush is exercised.
	cancel()
	<-done
	reports.Close()
	deadline := time.Now().Add(90 * time.Second)
	for {
		response := request("GET", "/v1/admin/sentry/references/"+ref+"/signals?period=1h", "", false)
		if response.Code != 200 {
			t.Fatalf("correlation HTTP %d", response.Code)
		}
		var result struct {
			Data operatorreporting.ReferenceSignals `json:"data"`
		}
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal("invalid correlation")
		}
		found := map[string]bool{}
		for _, s := range result.Data.Signals {
			found[s.Component+":"+s.Dataset] = true
		}
		required := []string{"paperboat-server"}
		if os.Getenv("PAPERBOAT_LIVE_SENTRY_ALL_COMPONENTS") == "true" {
			required = append(required, "paperboat-relay", "paperboat-tunnel", "paperboat-cli", "paperboat-daemon")
		}
		complete := true
		for _, component := range required {
			for _, dataset := range []string{"errors", "logs", "spans"} {
				complete = complete && found[component+":"+dataset]
			}
		}
		if complete {
			t.Logf("correlated errors/logs/spans across %d components reference=%s", len(required), ref)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sentry indexing incomplete datasets=%v", found)
		}
		time.Sleep(3 * time.Second)
	}
	response := request("GET", "/v1/admin/sentry/metrics?metric=paperboat.runtime.goroutines&component=paperboat-server&period=1h&region=validation&instance=slot-01", "", false)
	if response.Code != 200 {
		t.Fatalf("scoped metrics HTTP %d", response.Code)
	}
	var series struct {
		Data operatorreporting.MetricSeries `json:"data"`
	}
	if json.Unmarshal(response.Body.Bytes(), &series) != nil {
		t.Fatal("invalid metric series")
	}
	observed := false
	for _, point := range series.Data.Points {
		if point.Value == 42 {
			observed = true
		}
	}
	if series.Data.Scope != "deployment_slot" || series.Data.Region != "validation" || series.Data.Instance != "slot-01" || !observed {
		t.Fatal("scoped observed metric missing")
	}
	var count int
	if err = store.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND event_type='operator_reporting.read'`, operatorID).Scan(&count); err != nil || count < 5 {
		t.Fatal("read audit missing")
	}
	if _, err = store.Pool().Exec(ctx, `UPDATE operator_accounts SET status='disabled' WHERE id=$1`, operatorID); err != nil {
		t.Fatal(err)
	}
	if response := request("GET", "/v1/admin/sentry/monitors?project=go", "", false); response.Code != 403 {
		t.Fatal("revoked operator disclosed monitors")
	}
	t.Logf("verified %d reporting audits, fresh authentication and immediate role revocation", count)
	// Append-only audit and actors are removed by the runner dropping its task DB.
}
