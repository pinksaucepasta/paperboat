package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat/server/internal/browseringress"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/endpoints"
	"github.com/pinksaucepasta/paperboat/server/internal/lazyaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/metering"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat/server/internal/peersessions"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/previewdispatch"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
	"github.com/pinksaucepasta/paperboat/server/internal/testauth"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// Run only on the designated Hetzner test host. The client machines exercise
// actual machine setup and private access against this router and isolated database.
// Only the upstream identity provider is substituted; no principal or machine
// credential is forged, and all enrollment, signatures and observations are real.
func TestTask368ServeMachineServices(t *testing.T) {
	directory := os.Getenv("PAPERBOAT_TASK368_MACHINE_DIR")
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
	address := os.Getenv("PAPERBOAT_TASK368_MACHINE_LISTEN")
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !strings.HasPrefix(host, "100.") {
		t.Fatal("private Tailscale listener and HTTPS origin required")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	base := "https://" + listener.Addr().String()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancelFixture := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancelFixture()
	// The prepared isolated database is migrated once by the owning scenario.
	var schemaReady bool
	if err := database.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.machine_service_snapshots') IS NOT NULL`).Scan(&schemaReady); err != nil || !schemaReady {
		t.Fatal("prepared machine-service schema required")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	secret := base64.RawURLEncoding.EncodeToString(key)
	clear(key)
	cfg := config.Default()
	cfg.HTTP.PublicBaseURL = base
	cfg.Preview.BaseDomain = "preview.example.test"
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
	machines := usermachines.New(database, writer, usermachines.Policy{PairingLifetime: cfg.UserMachines.PairingLifetime, OfflineAfter: cfg.UserMachines.OfflineAfter, AllowedPlatforms: cfg.UserMachines.AllowedPlatforms}, billingService)
	machines.ConfigureInstallationEncryption(secret)
	machines.ConfigureAccess(nil, base)
	machines.ConfigureFileTransfer(accessdescriptor.FileTransferPolicy(cfg.Access.FileTransfer.Published()))
	machines.ConfigureCLIAuth(cfg.CLIAuth.ClientID, cfg.CLIAuth.AllowedScopes, cfg.CLIAuth.AccessTokenLifetime, cfg.CLIAuth.RefreshTokenLifetime, secret)
	machines.ConfigureMachineControl(signer, base)
	if err := machines.ConfigureRuntimeRoute("task368.invalid", 38080); err != nil {
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
	network, err := peersessions.NewNetworkService(database, signer, base)
	if err != nil {
		t.Fatal(err)
	}
	nativeResolver, err := nativeprivateaccess.NewSQLResolver(database)
	if err != nil {
		t.Fatal(err)
	}
	previewStore, err := previewtunnelstore.New(database)
	if err != nil {
		t.Fatal(err)
	}
	previews, err := previewv1.NewService(previewStore, previewv1.Config{EndpointDomain: cfg.Preview.BaseDomain, CursorKey: []byte(secret)[:32], AttachmentReadiness: endpoints.PreviewReadiness{DB: database}})
	if err != nil {
		t.Fatal(err)
	}
	previewAPI, err := previewtunnelapi.NewService(previewStore, []byte(secret)[:32])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	ca, err := os.ReadFile(os.Getenv("PAPERBOAT_TASK368_TLS_CA"))
	if err != nil || !roots.AppendCertsFromPEM(ca) {
		t.Fatal("fixture trust root required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	defer transport.CloseIdleConnections()
	dispatcher, err := previewdispatch.New(previewdispatch.Config{Resolver: previewdispatch.DBMachineRouteResolver{DB: database}, Signer: signer, Issuer: base, Client: &http.Client{Transport: transport}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	previews.ConfigureDispatcher(task368ObservedDispatch{t: t, next: dispatcher})
	lazy := &lazyaccess.Activator{DB: database, Leases: previews}
	policies, err := lazyaccess.NewService(database, cfg.Preview.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := browseraccess.NewService(database, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	browser.MachineServiceDomain = cfg.Preview.BaseDomain
	machineVerifier, err := NewPreviewAttachmentMachineProofVerifier(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	edgeVerifier, err := previewattachment.NewDBPreviewEdgeRequestVerifier(database, secret)
	if err != nil {
		t.Fatal(err)
	}
	browserHandlers := &BrowserAccessHandlers{Lazy: lazy, Access: browser, Ingress: &browseringress.Service{DB: database, Access: browser, Quota: billingService}, Edge: edgeVerifier, Machine: machineVerifier}
	machineServices := machineservices.New(database, "local.example.test")
	router := authTestOriginHandler{next: NewRouter(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: accounts, DeviceAuth: cliAuth, WorkspaceBilling: billingService, Machines: machines, MintKeys: signer, MachineServices: machineServices, NativePrivateAccess: &nativeprivateaccess.Service{Resolver: nativeResolver, Signer: signer, Issuer: base}, Enrollment: enrollment, RuntimeIdentity: enrollment, PeerIdentity: identities, PeerNetwork: network, LazyActivation: lazy, LazyPolicies: policies, PreviewLeases: previews, PreviewTunnelAPI: previewAPI, BrowserAccess: browserHandlers, MeteringRepo: metering.NewRuntimeRepository(database, cfg.UserMachines.OfflineAfter/2, cfg.ConfigSync.StaleHeartbeatAfter)})}
	type actor struct {
		Name      string         `json:"name"`
		AccountID string         `json:"account_id"`
		Token     tokenResponse  `json:"tokens"`
		Cookies   []*http.Cookie `json:"cookies"`
	}
	var actors []actor
	for _, name := range []string{"owner", "outsider"} {
		email := "pb368-" + name + "-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test"
		cookies := loginCookies(t, router, "machine_service_"+email+":"+email+":"+name)
		account := userIDByEmail(t, database, email)
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',3,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_machine_service_"+account, account, "sub_machine_service_"+account); err != nil {
			t.Fatal(err)
		}
		actors = append(actors, actor{Name: name, AccountID: account, Token: authorizeCLI(t, router, cookies), Cookies: cookies})
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No external test-provider sign-in is exposed after fixture accounts exist.
		if strings.HasPrefix(r.URL.Path, "/v1/auth/workos/") {
			http.NotFound(w, r)
			return
		}
		router.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	finished := make(chan error, 1)
	go func() {
		finished <- server.ServeTLS(listener, os.Getenv("PAPERBOAT_TASK368_TLS_CERT"), os.Getenv("PAPERBOAT_TASK368_TLS_KEY"))
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	keys := httptest.NewRecorder()
	signer.ServeHTTP(keys, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	payload, err := json.Marshal(struct {
		ControlURL            string          `json:"control_url"`
		Issuer                string          `json:"issuer"`
		JWKS                  json.RawMessage `json:"jwks"`
		BrowserDomain         string          `json:"browser_domain"`
		EdgeControlCredential string          `json:"edge_control_credential"`
		Actors                []actor         `json:"actors"`
		ExpiresAt             time.Time       `json:"expires_at"`
	}{base, base, json.RawMessage(keys.Body.Bytes()), cfg.Preview.BaseDomain, secret, actors, time.Now().UTC().Add(15 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "fixture.json"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	clear(payload)
	t.Log("private authenticated machine-service fixture ready")
	defer database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id IN ('task368-relay','task368-browser-edge')`)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-finished:
			t.Fatalf("private enrollment listener stopped: %v", err)
		case <-ticker.C:
			task368RegisterBrowserEdge(t, ctx, database.SQL(), directory, actors[0].AccountID)
			task368RegisterBrowserTarget(t, ctx, database.SQL(), directory, actors[0].AccountID)
			// The relay test process writes this protected local heartbeat only
			// while its production UDP listener reports Ready(). No node is
			// admitted from a reserved socket, desired config or stale marker.
			if raw, e := os.ReadFile(filepath.Join(directory, "relay-ready.json")); e == nil && len(raw) < 4096 {
				var ready struct {
					NodeID   string    `json:"node_id"`
					Epoch    string    `json:"process_epoch"`
					UDP      int       `json:"udp_port"`
					TCP      int       `json:"tcp_port"`
					Observed time.Time `json:"observed_at"`
					Ready    bool      `json:"ready"`
				}
				if json.Unmarshal(raw, &ready) == nil && ready.NodeID == "task368-relay" && ready.Ready && ready.Epoch != "" && ready.UDP > 0 && ready.UDP < 65536 && ready.TCP > 0 && ready.TCP < 65536 && ready.TCP != ready.UDP && ready.Observed.After(time.Now().Add(-5*time.Second)) && !ready.Observed.After(time.Now().Add(time.Second)) {
					if _, e = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,registry_expires_at,capacity_limit,capacity_used,capacity_observed_at,allowed_account_ids) VALUES('task368-relay','task368','v1',$1,'100.70.146.64',$2,$3,'ready',true,$4,1,'task368','hetzner',ARRAY['relay'],ARRAY['derp_quic','derp_wss'],$4::timestamptz+interval '15 seconds',100,0,$4,ARRAY[$5]::text[]) ON CONFLICT(id) DO UPDATE SET process_epoch=EXCLUDED.process_epoch,endpoint_tcp_port=EXCLUDED.endpoint_tcp_port,endpoint_quic_port=EXCLUDED.endpoint_quic_port,state='ready',ready=true,last_heartbeat_at=EXCLUDED.last_heartbeat_at,registry_expires_at=EXCLUDED.registry_expires_at,capacity_observed_at=EXCLUDED.capacity_observed_at,allowed_account_ids=EXCLUDED.allowed_account_ids`, ready.Epoch, ready.TCP, ready.UDP, ready.Observed, actors[0].AccountID); e != nil {
						t.Fatal("live relay registration:", e)
					}
				}
			}
			if _, err := os.Stat(filepath.Join(directory, "stop")); err == nil {

				return
			}
		}
	}
}
