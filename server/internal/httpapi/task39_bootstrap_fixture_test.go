package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/lazyaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/managedssh"
	"github.com/pinksaucepasta/paperboat-server/internal/metering"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat-server/internal/peersessions"
	"github.com/pinksaucepasta/paperboat-server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/runtimeattachment"
	"github.com/pinksaucepasta/paperboat-server/internal/testauth"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

// Run only on the designated Hetzner test host. The client machines exercise
// actual one-shot bootstrap against this private router and isolated database.
// Only the upstream identity provider is substituted; no principal or machine
// credential is forged, and all enrollment, signatures and observations are real.
func TestTask39ServeAuthenticatedEnrollment(t *testing.T) {
	directory := os.Getenv("PAPERBOAT_TASK39_BOOTSTRAP_DIR")
	if directory == "" {
		t.Skip("private native enrollment fixture is opt-in")
	}
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("absolute protected fixture directory required")
	}
	address, base := os.Getenv("PAPERBOAT_TASK39_BOOTSTRAP_LISTEN"), os.Getenv("PAPERBOAT_TASK39_BOOTSTRAP_URL")
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !strings.HasPrefix(host, "100.") || !strings.HasPrefix(base, "https://") {
		t.Fatal("private Tailscale listener and HTTPS origin required")
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	secret := base64.RawURLEncoding.EncodeToString(key)
	clear(key)
	cfg := config.Default()
	cfg.HTTP.PublicBaseURL = base
	writer := audit.NewWriter(database)
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	accounts := auth.NewService(database, writer, testauth.FakeWorkOSVerifier{}, []string{secret}, true, "https://login.example.test")
	cliAuth := auth.NewDeviceService(database, writer, cfg.CLIAuth, []string{secret})
	cliAuth.ConfigureAuthorizationEncryptionKey("test-device-recovery-key")
	billingService := billing.NewWorkspaceService(database, nil, secret)
	if err := billingService.InitializeAllowancePolicy(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	machines := usermachines.New(database, writer, usermachines.Policy{PairingLifetime: 30 * time.Minute, OfflineAfter: cfg.UserMachines.OfflineAfter, AllowedPlatforms: cfg.UserMachines.AllowedPlatforms}, billingService)
	machines.ConfigureInstallationEncryption(secret)
	machines.ConfigureAccess(nil, base)
	machines.ConfigureFileTransfer(accessdescriptor.FileTransferPolicy{
		Revision: cfg.Access.FileTransfer.Revision, MaxFileBytes: cfg.Access.FileTransfer.MaxFileBytes,
		MaxBatchFiles: cfg.Access.FileTransfer.MaxBatchFiles, MaxBatchBytes: cfg.Access.FileTransfer.MaxBatchBytes,
		MaxConcurrentTransfers: cfg.Access.FileTransfer.MaxConcurrentTransfers, RetentionSeconds: int64(cfg.Access.FileTransfer.Retention / time.Second),
		DeliveryTimeoutSeconds: int64(cfg.Access.FileTransfer.DeliveryTimeout / time.Second), MaxPendingSpoolBytes: cfg.Access.FileTransfer.MaxPendingSpoolBytes,
	})
	machines.ConfigureCLIAuth(cfg.CLIAuth.ClientID, cfg.CLIAuth.AllowedScopes, cfg.CLIAuth.AccessTokenLifetime, cfg.CLIAuth.RefreshTokenLifetime, secret)
	machines.ConfigureMachineControl(signer, base)
	helperPort := int64(38080)
	if raw := strings.TrimSpace(os.Getenv("PAPERBOAT_TASK39_HELPER_PORT")); raw != "" {
		var err error
		helperPort, err = strconv.ParseInt(raw, 10, 32)
		if err != nil {
			t.Fatal("invalid helper port")
		}
	}
	if err := machines.ConfigureRuntimeRoute("task39.invalid", int32(helperPort)); err != nil {
		t.Fatal(err)
	}

	enrollment := controlplane.NewEnrollmentService(database, signer, writer, base, secret)
	machines.ConfigureHelperEnrollment(func(ctx context.Context, actor, operation, environment string, lifetime time.Duration) (usermachines.HelperEnrollmentGrant, error) {
		grant, err := enrollment.Issue(ctx, actor, operation, environment, lifetime)
		return usermachines.HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, err
	})
	machines.ConfigureHelperRecovery(func(ctx context.Context, actor, operation, environment, helper string, lifetime time.Duration) (usermachines.HelperEnrollmentGrant, error) {
		grant, err := enrollment.RecoverHelper(ctx, actor, operation, environment, helper, lifetime)
		return usermachines.HelperEnrollmentGrant{EnrollmentID: grant.EnrollmentID, HelperID: grant.HelperID, Credential: grant.Credential, ExpiresAt: grant.ExpiresAt}, err
	})
	identityRepo, err := peeridentity.NewSQLRepository(database, writer)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := peeridentity.NewService(identityRepo)
	if err != nil {
		t.Fatal(err)
	}
	network, err := peersessions.NewNetworkService(database, signer, base, peersessions.NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sshRepository, err := managedssh.NewSQLRepository(database, writer)
	if err != nil {
		t.Fatal(err)
	}
	sshService, err := managedssh.NewService(sshRepository)
	if err != nil {
		t.Fatal(err)
	}
	machineVerifier, err := NewPreviewAttachmentMachineProofVerifier(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	edgeVerifier, err := previewattachment.NewDBPreviewEdgeRequestVerifier(database, secret)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCarrier := &runtimeattachment.Handler{Service: &runtimeattachment.Service{DB: database}, Machine: machineVerifier, Edge: edgeVerifier}
	router := authTestOriginHandler{next: NewRouter(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: accounts, DeviceAuth: cliAuth, WorkspaceBilling: billingService, Machines: machines, MintKeys: signer, Enrollment: enrollment, RuntimeIdentity: enrollment, PeerIdentity: identities, PeerNetwork: network, ManagedSSH: sshService, MachineServices: machineservices.New(database, "local.example.test"), RuntimeCarrierAttachment: runtimeCarrier, LazyActivation: &lazyaccess.Activator{DB: database}, MeteringRepo: metering.NewRuntimeRepository(database, secret, cfg.ConfigSync.StaleHeartbeatAfter)})}
	type user struct {
		Name         string `json:"name"`
		AccountID    string `json:"account_id"`
		EnrollmentID string `json:"enrollment_id"`
		Token        string `json:"token"`
	}
	var users []user
	names := []string{"pb39a", "pb39b", "pb39c"}
	if target := os.Getenv("PAPERBOAT_TASK39_BOOTSTRAP_TARGET"); target != "" {
		if target != "pb39c" {
			t.Fatal("single-target fixture supports the Linux account only")
		}
		names = []string{target}
	}
	for _, name := range names {
		email := name + "-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test"
		cookies := loginCookies(t, router, "bootstrap_"+email+":"+email+":"+name)
		account := userIDByEmail(t, database, email)
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_bootstrap_"+account, account, "sub_bootstrap_"+account); err != nil {
			t.Fatal(err)
		}
		// An authenticated account issues the one-shot token through the same
		// service used by the already-tested HTTP enrollment endpoint.
		shell := "posix"
		if name == "pb39b" {
			shell = "powershell"
		}
		started, err := machines.StartEnrollmentWithOptions(ctx, account, "bootstrap_"+name, usermachines.EnrollmentOptions{Shell: shell})
		if err != nil {
			t.Fatal(err)
		}
		if len(cookies) == 0 {
			t.Fatal("authenticated account session missing")
		}
		users = append(users, user{Name: name, AccountID: account, EnrollmentID: started.ID, Token: started.BootstrapToken})
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	httpLog, err := os.OpenFile(filepath.Join(directory, "http-status.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer httpLog.Close()
	requestLogger := slog.New(slog.NewJSONHandler(httpLog, nil))
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No external test-provider sign-in is exposed after fixture accounts exist.
		if strings.HasPrefix(r.URL.Path, "/v1/auth/workos/") {
			http.NotFound(w, r)
			return
		}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		router.ServeHTTP(recorder, r)
		pattern := r.Pattern
		if pattern == "" {
			pattern = "unknown"
		}
		requestLogger.Info("fixture_http", "method", r.Method, "route", pattern, "status", recorder.status)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	finished := make(chan error, 1)
	go func() {
		finished <- server.ServeTLS(listener, os.Getenv("PAPERBOAT_TASK39_TLS_CERT"), os.Getenv("PAPERBOAT_TASK39_TLS_KEY"))
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	payload, err := json.Marshal(users)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "enrollments.json"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	clear(payload)
	t.Log("private authenticated enrollment fixture ready")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-finished:
			t.Fatalf("private enrollment listener stopped: %v", err)
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(directory, "stop")); err == nil {
				for _, user := range users {
					var count int
					if err := database.Pool().QueryRow(ctx, `SELECT count(*) FROM paperboat.user_machines WHERE user_id=$1 AND online AND deleted_at IS NULL AND revoked_at IS NULL`, user.AccountID).Scan(&count); err != nil || count != 1 {
						t.Fatalf("expected one ready enrolled machine for %s: count=%d err=%v", user.Name, count, err)
					}
				}
				return
			}
		}
	}
}

// An installed runtime always sends the bounded machine-services snapshot,
// including when it has no ports to announce. The native fixture must supply
// the same durable sink as production before presence can become ready.
func TestTask39SignedHeartbeatRequiresMachineServices(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	account, machine, environment, enrollmentID := "usr_fixture_observe_"+suffix, "machine_fixture_observe_"+suffix, "env_fixture_observe_"+suffix, "pairing_fixture_observe_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, environment, machine, account)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'fixture-observation','linux','amd64','/workspace','offline','occupied')`, machine, account, environment)
	exec(`INSERT INTO paperboat.user_machine_pairings(id,bootstrap_owner_account,operation_id,idempotency_key,bootstrap_token_hash,bootstrap_token_ciphertext,state,installation_state,generation,user_machine_id,verifier_hash,user_code,requested_alias,platform,architecture,workspace_root,public_identity_key,expires_at)
VALUES($1,$2,$1,$1,$3,$3,'consumed','installing',1,$4,$3,'OBSERVE123','fixture-observation','linux','amd64','/workspace','AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',now()+interval '10 minutes')`, enrollmentID, account, []byte("unused-fixture-bootstrap"), machine)
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer := "https://fixture.example.test"
	writer := audit.NewWriter(database)
	identities := controlplane.NewEnrollmentService(database, signer, writer, issuer, "fixture-observation-encryption")
	grant, err := identities.Issue(ctx, account, "fixture-observation-enrollment-"+suffix, environment, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	machines := usermachines.New(database, writer, usermachines.Policy{}, nil)
	options := Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Enrollment: identities, RuntimeIdentity: identities, Machines: machines, MeteringRepo: metering.NewRuntimeRepository(database, "unused")}
	router := NewRouter(options)
	exchangeBody, err := json.Marshal(map[string]string{"credential": grant.Credential, "public_key": base64.RawURLEncoding.EncodeToString(public)})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/helper-enrollments", bytes.NewReader(exchangeBody))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("actual helper exchange status=%d", response.Code)
	}
	var exchanged struct {
		Data controlplane.HelperIdentity `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &exchanged); err != nil || exchanged.Data.Credential == "" || exchanged.Data.MachineID != machine {
		t.Fatal("actual helper identity binding failed")
	}
	now := time.Now().UTC()
	body, err := json.Marshal(map[string]any{
		"environment_id": environment, "resource_id": machine, "sampled_at": now, "reporter_version": "2026.10.03.990",
		"machine_services":    wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "process-fixture-observation", StartedAt: now, Generation: 1, Services: []wire.Port{}},
		"runtime_diagnostics": map[string]any{"capabilities": []string{"file_receive", "preview_launch", "terminal_host", "session_host", "ssh_host", "keep_awake"}, "worker_generation": 1, "os_boot_id": "fixture-boot", "worker_service_scope": "user", "connector_state": "unavailable", "connector_generation": 0, "observed_at": now},
	})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := func(handler http.Handler, operation string, tamper bool) int {
		t.Helper()
		hash := sha256.Sum256(body)
		claims := controlplane.HelperProofClaims{HelperID: exchanged.Data.HelperID, EnvironmentID: environment, OperationID: operation, Method: http.MethodPost, Path: "/v1/runtime-observations", BodySHA256: base64.RawURLEncoding.EncodeToString(hash[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := json.Marshal(map[string]string{"alg": "EdDSA", "payload": base64.RawURLEncoding.EncodeToString(payload), "signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
		if err != nil {
			t.Fatal(err)
		}
		sentBody := body
		if tamper {
			sentBody = append(append([]byte{}, body...), byte(' '))
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", bytes.NewReader(sentBody))
		request.Header.Set("Authorization", "Bearer "+exchanged.Data.Credential)
		request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	assertState := func(wantOnline bool, wantGeneration int, wantEnrollment string) {
		t.Helper()
		var online bool
		var generation int
		var state string
		if err := database.SQL().QueryRowContext(ctx, `SELECT m.online,m.worker_generation,e.installation_state FROM paperboat.user_machines m JOIN paperboat.user_machine_pairings e ON e.user_machine_id=m.id WHERE m.id=$1 AND e.id=$2`, machine, enrollmentID).Scan(&online, &generation, &state); err != nil {
			t.Fatal(err)
		}
		if online != wantOnline || generation != wantGeneration || state != wantEnrollment {
			t.Fatalf("presence online=%v generation=%d enrollment=%s", online, generation, state)
		}
	}
	if status := heartbeat(router, "fixture-missing-services-"+suffix, false); status != http.StatusServiceUnavailable {
		t.Fatalf("missing service status=%d", status)
	}
	assertState(false, 0, "installing")
	options.MachineServices = machineservices.New(database, "local.example.test")
	router = NewRouter(options)
	if status := heartbeat(router, "fixture-invalid-proof-"+suffix, true); status != http.StatusUnauthorized {
		t.Fatalf("tampered proof status=%d", status)
	}
	assertState(false, 0, "installing")
	if status := heartbeat(router, "fixture-wired-services-"+suffix, false); status != http.StatusAccepted {
		t.Fatalf("wired service status=%d", status)
	}
	assertState(true, 1, "ready")
}
