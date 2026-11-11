package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

// TestE2EServe is the server half of the CI end-to-end job (tools/e2e/run.sh): the real application
// (app.New and Run, every route and worker) on PostgreSQL behind a TLS proxy, with one seeded account
// that owns a signed-in browser session. It issues the account's enrollment tokens and API token over
// HTTP like the console does, writes them to PAPERBOAT_E2E_DIR/tokens.json, and runs until
// PAPERBOAT_E2E_DIR/stop exists or SIGTERM (a deploy's graceful stop). Started again with tokens.json present it
// only serves: TestE2ECore restarts it mid-run like a deploy.
func TestE2EServe(t *testing.T) {
	directory := os.Getenv("PAPERBOAT_E2E_DIR")
	if directory == "" {
		t.Skip("CI end-to-end server only")
	}
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	base, listen := os.Getenv("PAPERBOAT_E2E_URL"), os.Getenv("PAPERBOAT_E2E_LISTEN")
	internal := os.Getenv("PAPERBOAT_E2E_INTERNAL")
	if internal == "" {
		internal = "127.0.0.1:18080"
	}
	cfg := config.Default()
	cfg.Database = config.Database{Driver: "postgres", DSN: dsn}
	cfg.HTTP.Address, cfg.HTTP.PublicBaseURL, cfg.HTTP.BrowserLoginOrigin = internal, base, base
	cfg.ReleaseBaseURL = base
	// One mint key across processes, as production keeps it: the multi stage's instances and TestE2ECore's restart
	// (an ephemeral key per start would invalidate every grant the daemons hold).
	if key := os.Getenv("PAPERBOAT_E2E_MINT_KEY"); key != "" {
		cfg.Secrets.MintSigningKeys, cfg.CLIAuth.MintActiveKeyID = []string{key}, strings.SplitN(key, ":", 2)[0]
	}
	if os.Getenv("PAPERBOAT_E2E_INSTANCES") == "3" { // multi stage
		cfg.ServerInstances = 3
	}
	cfg.CLIAuth.VerificationURL, cfg.CLIAuth.MachinesURL = base+"/cli", base+"/machines"
	cfg.LocalAccessBaseDomain, cfg.RuntimeBaseDomain = "local.example.test", "runtime.example.test"
	// One public base, as production: the managed wildcard certificate covers both.
	cfg.Preview.BaseDomain, cfg.Tunnel.BaseDomain = "public.example.test", "public.example.test"
	// TestE2ECore's edge: claimed by the operator, certificates from Pebble.
	if acme := os.Getenv("PAPERBOAT_E2E_ACME_URL"); acme != "" {
		e2eEdgeConfig(t, &cfg, acme)
	}
	migrations, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), migrations); err != nil {
		t.Fatal(err)
	}
	defer migrations.Close()
	opts := Options{Config: cfg}
	if os.Getenv("PAPERBOAT_E2E_ACME_URL") != "" {
		opts.CertificateRuntime = e2eCertificateRuntime(t, migrations, cfg)
	}
	opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	application, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	running := make(chan error, 1)
	go func() { running <- application.Run(ctx) }()

	waitForStop := func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for ; ; <-tick.C {
			select {
			case err := <-running:
				if ctx.Err() != nil { // SIGTERM: Run drained and returned
					return
				}
				t.Fatalf("application stopped: %v", err)
			default:
			}
			if _, err := os.Stat(filepath.Join(directory, "stop")); err == nil {
				return
			}
		}
	}
	_, statErr := os.Stat(filepath.Join(directory, "tokens.json"))
	seeded := statErr == nil
	if os.Getenv("PAPERBOAT_E2E_FOLLOWER") != "" { // another instance of the multi stage: serve only
		waitForStop()
		return
	}
	if listen != "" { // single stage: TLS in front of the one instance; the multi stage runs TestE2EProxy
		target, _ := url.Parse("http://" + internal)
		tlsServer := &http.Server{Addr: listen, Handler: logRequests(t, httputil.NewSingleHostReverseProxy(target)), ReadHeaderTimeout: 5 * time.Second,
			TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
		go func() {
			_ = tlsServer.ListenAndServeTLS(os.Getenv("PAPERBOAT_E2E_TLS_CERT"), os.Getenv("PAPERBOAT_E2E_TLS_KEY"))
		}()
		defer tlsServer.Close()
	}

	if seeded { // a restart: the account and tokens exist
		waitForStop()
		return
	}
	// A signed-in console session for a seeded account, as perf_budgets does, plus a paid entitlement.
	hash := func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }
	account, session, csrf := "usr_e2e", "e2e-session", "e2e-csrf"
	for _, statement := range []string{
		`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('usr_e2e','workos_e2e','e2e@example.test','active')`,
		`INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ('ume_e2e','usr_e2e','sub_e2e','connected-test','active',3,1073741824,now()-interval '1 hour',now()+interval '1 day')`,
	} {
		if _, err := application.db.SQL().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := application.db.SQL().ExecContext(ctx, `INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at,workos_session_id,workos_access_expires_at) VALUES('session_e2e',$1,$2,$3,now()+interval '1 day','session_e2e',now()+interval '1 day')`, account, hash(session), hash(csrf)); err != nil {
		t.Fatal(err)
	}
	// A signed-in operator (e2eOperatorSession) claims TestE2ECore's edge as a managed one.
	if _, err := application.db.SQL().ExecContext(ctx, `WITH a AS (INSERT INTO paperboat.operator_accounts(id,email,password_hash) VALUES('opr_`+strings.Repeat("e2", 16)+`','operator@example.test','unused') RETURNING id)
		INSERT INTO paperboat.operator_sessions(token_hash,operator_id,csrf_hash,expires_at) SELECT $1,id,$2,now()+interval '1 day' FROM a`, hashBytes(e2eOperatorSession), hashBytes(e2eOperatorCSRF)); err != nil {
		t.Fatal(err)
	}
	post := func(path, body string) map[string]any {
		t.Helper()
		var response *http.Response
		for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(time.Second) {
			request, _ := http.NewRequest(http.MethodPost, "http://"+internal+path, strings.NewReader(body))
			request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
			request.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
			request.Header.Set(auth.CSRFHeaderName, csrf)
			request.Header.Set("Origin", base)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", hash(path+body+time.Now().String()))
			if r, err := http.DefaultClient.Do(request); err == nil {
				response = r
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("server did not answer: %v", err)
			}
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var out struct{ Data map[string]any }
		if response.StatusCode >= 300 || json.NewDecoder(bytes.NewReader(raw)).Decode(&out) != nil {
			t.Fatalf("POST %s: status %d: %.300s", path, response.StatusCode, raw)
		}
		return out.Data
	}
	tokens := map[string]string{}
	for _, name := range []string{"e2e-a", "e2e-b", "e2e-c"} {
		tokens[name], _ = post("/v1/machine-enrollments", `{}`)["bootstrap_token"].(string)
	}
	for name, value := range tokens {
		if value == "" {
			t.Fatalf("no %s issued", name)
		}
	}
	payload, _ := json.Marshal(tokens)
	if err := os.WriteFile(filepath.Join(directory, "tokens.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForStop()
}

// The seeded operator's session cookie and CSRF token (fixtures of a throwaway database).
const e2eOperatorSession, e2eOperatorCSRF = "e2e-operator-session", "e2e-operator-csrf"

func hashBytes(value string) []byte { sum := sha256.Sum256([]byte(value)); return sum[:] }

// e2eEdgeConfig turns on what a managed edge needs from the server: the edge control credential, private claim
// addresses (the edge is on the CI host), and managed certificates over http-01 from a Pebble ACME directory whose
// validation always passes. The secrets go to the environment references the server resolves.
func e2eEdgeConfig(t *testing.T, cfg *config.Config, acme string) {
	credential := os.Getenv("PAPERBOAT_E2E_EDGE_CREDENTIAL")
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	accountDER, err := x509.MarshalPKCS8PrivateKey(accountKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SelfhostAllowPrivateIngress = true
	cfg.Secrets.EdgeControlCredential = credential
	cfg.Certificates = config.Certificates{Enabled: true, DirectoryURL: acme, Issuer: "pebble", Challenge: config.CertificateChallengeHTTP01,
		AccountKeyReference: "e2e-account", MasterKeyReference: "e2e-master", DistributionCredentialReference: "e2e-distribution",
		OwnerID: "e2e", CAAResolver: os.Getenv("PAPERBOAT_E2E_CAA_RESOLVER")}
	for reference, value := range map[string]string{
		"e2e-account":      string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: accountDER})),
		"e2e-master":       credential + "-master", // the same on every start: it seals the stored certificate keys
		"e2e-distribution": credential,
	} {
		name, err := tunnelcert.EnvironmentReferenceName("", reference)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, value)
	}
}

// logRequests prints one line per request so a failing run shows which route answered what.
func logRequests(t *testing.T, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		t.Logf("request method=%s path=%s status=%d %s", r.Method, r.URL.Path, recorder.status, recorder.failure.String())
	})
}

// statusRecorder keeps flushing and connection upgrades working through ResponseController.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	failure bytes.Buffer // the first bytes of an error body: its code names the refusal
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status >= 400 && r.failure.Len() < 300 {
		r.failure.Write(b[:min(len(b), 300)])
	}
	return r.ResponseWriter.Write(b)
}
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
