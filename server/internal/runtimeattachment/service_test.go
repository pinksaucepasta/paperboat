package runtimeattachment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type machineVerifierFunc func(context.Context, *http.Request, []byte) (previewattachment.MachineProof, error)

func (f machineVerifierFunc) VerifyMachineRequest(ctx context.Context, r *http.Request, body []byte) (previewattachment.MachineProof, error) {
	return f(ctx, r, body)
}

type edgeVerifierFunc func(context.Context, *http.Request, []byte) (previewattachment.PreviewEdgeIdentity, error)

func (f edgeVerifierFunc) VerifyPreviewEdgeRequest(ctx context.Context, r *http.Request, body []byte) (previewattachment.PreviewEdgeIdentity, error) {
	return f(ctx, r, body)
}

func TestRuntimeCarrierVerifierFailuresKeepExpectedDenialsAndReportOutages(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previousLogger)

	const canary = "PRIVATE_MACHINE_ERROR_CANARY"
	outage := errors.Join(previewattachment.ErrUnauthorized, fmt.Errorf("%s: %w", canary, syscall.EIO))
	tests := []struct {
		name, path, body, operation string
		handler                     *Handler
		wantStatus                  int
		wantCode                    string
		wantReport                  bool
	}{
		{
			name: "machine proof outage",
			path: runtimewire.HostPath,
			body: `{}`,
			handler: &Handler{Machine: machineVerifierFunc(func(context.Context, *http.Request, []byte) (previewattachment.MachineProof, error) {
				return previewattachment.MachineProof{}, outage
			})},
			operation:  "http_other",
			wantStatus: http.StatusServiceUnavailable,
			wantReport: true,
		},
		{
			name: "edge verifier outage",
			path: runtimewire.EdgePullPath,
			body: `{"edge_node_id":"edge_1","process_epoch":"epoch_1"}`,
			handler: &Handler{Edge: edgeVerifierFunc(func(context.Context, *http.Request, []byte) (previewattachment.PreviewEdgeIdentity, error) {
				return previewattachment.PreviewEdgeIdentity{}, outage
			})},
			operation:  "http_edge",
			wantStatus: http.StatusServiceUnavailable,
			wantReport: true,
		},
		{
			name: "expected machine denial",
			path: runtimewire.HostPath,
			body: `{}`,
			handler: &Handler{Machine: machineVerifierFunc(func(context.Context, *http.Request, []byte) (previewattachment.MachineProof, error) {
				return previewattachment.MachineProof{}, previewattachment.ErrUnauthorized
			})},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "device_identity_invalid",
		},
		{
			name: "expected edge denial",
			path: runtimewire.EdgePullPath,
			body: `{"edge_node_id":"edge_1","process_epoch":"epoch_1"}`,
			handler: &Handler{Edge: edgeVerifierFunc(func(context.Context, *http.Request, []byte) (previewattachment.PreviewEdgeIdentity, error) {
				return previewattachment.PreviewEdgeIdentity{}, previewattachment.ErrUnauthorized
			})},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "edge_identity_invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs.Reset()
			reference := supportreport.NewReference()
			ctx := serverdiagnostics.WithCorrelationID(context.Background(), "request_runtime_attachment")
			ctx = supportreport.WithFailureIdentity(ctx, "", "request_runtime_attachment", reference)
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body)).WithContext(ctx)
			response := httptest.NewRecorder()
			test.handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			var payload struct {
				Error struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if test.wantCode != "" && payload.Error.Code != test.wantCode {
				t.Fatalf("response code = %q, want %q", payload.Error.Code, test.wantCode)
			}
			if test.wantReport {
				if payload.Error.Code != "runtime_carrier_unavailable" || !payload.Error.Retryable {
					t.Fatalf("outage response = %#v", payload.Error)
				}
				var event struct {
					Component        string `json:"component"`
					Operation        string `json:"operation"`
					Stage            string `json:"stage"`
					Code             string `json:"code"`
					Cause            string `json:"cause"`
					SupportReference string `json:"support_reference"`
					CorrelationID    string `json:"correlation_id"`
					Errno            int    `json:"errno"`
					HTTPStatus       int    `json:"http_status"`
				}
				if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
					t.Fatalf("expected one safe fault log: %v; logs=%q", err, logs.String())
				}
				if event.Component != "paperboat-server" || event.Operation != test.operation || event.Stage != "service_error" || event.Code != "carrier_unavailable" || event.Cause != "system_error" || event.Errno != int(syscall.EIO) || event.HTTPStatus != http.StatusServiceUnavailable || event.SupportReference != reference || event.CorrelationID != "request_runtime_attachment" {
					t.Fatalf("fault event = %#v", event)
				}
				if strings.Contains(logs.String(), canary) || strings.Contains(response.Body.String(), canary) {
					t.Fatal("private verifier error escaped into logs or response")
				}
			} else if logs.Len() != 0 {
				t.Fatalf("expected denial emitted an operational fault: %s", logs.String())
			}
		})
	}
}

func TestRuntimeAttachmentRejectsAmbiguousRequests(t *testing.T) {
	for _, body := range []string{`{"operation_id":"a","operation_id":"b"}`, `{"operation_id":"a","target":"127.0.0.1:22"}`, `{} {}`} {
		var request runtimewire.Request
		if decode([]byte(body), &request) == nil {
			t.Fatalf("accepted ambiguous request %s", body)
		}
	}
	p := previewattachment.MachineProof{UserID: "account_1", MachineID: "machine_1", OperationID: "operation_1", InstallationGeneration: 1}
	r := runtimewire.Request{OperationID: p.OperationID, ProcessID: "process_1", WorkerGeneration: 1, InstallationGeneration: 1, RuntimeListenPort: 38080}
	if !validRequest(p, r) {
		t.Fatal("valid request rejected")
	}
	r.InstallationGeneration++
	if validRequest(p, r) {
		t.Fatal("cross-installation accepted")
	}
	r.InstallationGeneration--
	r.RuntimeListenPort = 0
	if validRequest(p, r) {
		t.Fatal("missing listener port accepted")
	}
}

func TestRuntimeAttachmentAuthorityOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
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
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	prefix := "runtime_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	account, machine, env, route, node := prefix+"_account", prefix+"_machine", prefix+"_env", prefix+"_route", prefix+"_edge"
	backupNode := prefix + "_zbackup"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM selfhost_installations WHERE node_id=$1`, backupNode)
		for _, query := range []string{`DELETE FROM runtime_carrier_attachments WHERE route_id=$1`, `DELETE FROM control_routes WHERE id=$1`} {
			_, _ = database.SQL().ExecContext(ctx, query, route)
		}
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_connector_generations WHERE environment_id=$1`, env)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM user_machines WHERE id=$1`, machine)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_environments WHERE id=$1`, env)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, node)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, backupNode)
		_, _ = database.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, account)
	}()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.RawURLEncoding.EncodeToString(public)
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, env, account)
	exec(`INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation,worker_generation,configured_capabilities) VALUES ($1,$2,$3,'runtime-test','linux','amd64','/workspace','online','occupied',$4,1,1,ARRAY['terminal_host']::text[])`, machine, account, env, key)
	exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',38080)`, route, env, machine+".runtime.example.test")
	exec(`INSERT INTO control_connector_generations(environment_id,machine_id,connector_id,generation,edge_pool,state) VALUES($1,$2,'runtime',1,'default','pending')`, env, machine)
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem)
VALUES($1,'default','1.0','epoch_runtime','ready',true,now(),'first-region','first-domain',ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,$2,'fixture certificate')`, node, "sha256:"+strings.Repeat("1", 64))
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem)
VALUES($1,'default','1.0','epoch_backup','ready',true,now(),'second-region','second-domain',ARRAY['edge']::text[],ARRAY['http3','http2']::text[],128,0,now(),now()+interval '1 hour','backup.example.test',27443,27444,$2,'fixture certificate')`, backupNode, "sha256:"+strings.Repeat("2", 64))
	exec(`UPDATE control_tunnel_nodes SET public_ingress_ipv4=CASE WHEN id=$1 THEN '192.0.2.10'::inet ELSE '192.0.2.11'::inet END,public_ingress_verified_at=now() WHERE id IN ($1,$2)`, node, backupNode)
	service := &Service{DB: database}
	proof := previewattachment.MachineProof{UserID: account, MachineID: machine, OperationID: "operation_runtime", InstallationGeneration: 1}
	request := runtimewire.Request{OperationID: proof.OperationID, ProcessID: "process_runtime", WorkerGeneration: 1, InstallationGeneration: 1, RuntimeListenPort: 48080}
	edge := previewattachment.PreviewEdgeIdentity{NodeID: node, ProcessEpoch: "epoch_runtime"}
	a, err := service.Attach(ctx, proof, request)
	if err == nil && (a.Backup == nil || a.Backup.Binding.EdgeNodeID != backupNode) {
		t.Fatal("independent standby admission missing")
	}
	if err != nil {
		t.Fatal(err)
	}
	encodedHost, _ := json.Marshal(a)
	if strings.Contains(string(encodedHost), `"target"`) || a.Binding.HostID != machine || a.Binding.InstallationGeneration != 1 {
		t.Fatal("host admission contains a server-owned target or wrong machine")
	}
	var recordedPort int32
	if err := database.SQL().QueryRowContext(ctx, `SELECT target_port FROM control_routes WHERE id=$1`, route).Scan(&recordedPort); err != nil || recordedPort != request.RuntimeListenPort {
		t.Fatalf("recorded listener port=%d, error=%v", recordedPort, err)
	}
	ready := func(want bool) {
		t.Helper()
		var actual bool
		if err := database.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, route).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		if actual != want {
			t.Fatalf("ready=%v want %v", actual, want)
		}
	}
	ready(false)
	dnsArgs := dbsqlc.ListReadyRuntimeEdgeDNSAddressesV1Params{RouteID: route, Hostname: machine + ".runtime.example.test"}
	// dnsAddresses is what DNS would hold for the host: every edge with a fresh carrier, node -> IPv4.
	dnsAddresses := func() map[string]string {
		t.Helper()
		rows, err := database.Queries().ListReadyRuntimeEdgeDNSAddressesV1(ctx, dnsArgs)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, row := range rows {
			out[row.ID] = row.Ipv4
		}
		return out
	}
	if got := dnsAddresses(); len(got) != 0 {
		t.Fatalf("unobserved runtime edge was advertised in DNS: %v", got)
	}
	verifier, err := previewattachment.NewDBPreviewEdgeRequestVerifier(database, "runtime-test-edge-credential-0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{Service: service, Edge: verifier}
	for _, test := range []struct {
		credential, epoch string
		status            int
	}{{"wrong", "epoch_runtime", 401}, {"runtime-test-edge-credential-0000000000000000", "old_epoch", 401}, {"runtime-test-edge-credential-0000000000000000", "epoch_runtime", 200}} {
		body, _ := json.Marshal(map[string]string{"edge_node_id": node, "process_epoch": test.epoch})
		req := httptest.NewRequest(http.MethodPost, runtimewire.EdgePullPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+test.credential)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Fatalf("edge HTTP status=%d expected %d", response.Code, test.status)
		}
		if response.Code == 200 {
			var snapshot struct {
				Schema     string                  `json:"schema"`
				Complete   bool                    `json:"complete"`
				Admissions []runtimewire.Admission `json:"admissions"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.Schema != runtimewire.Schema || !snapshot.Complete || len(snapshot.Admissions) != 1 {
				t.Fatal("incomplete runtime snapshot")
			}
		}

	}
	admission, err := service.Pull(ctx, edge, nil)
	if err != nil || len(admission) != 1 {
		t.Fatalf("pull count=%d error=%v", len(admission), err)
	}
	raw, _ := json.Marshal(admission)
	if strings.Contains(string(raw), "127.0.0.1") || strings.Contains(string(raw), `"target"`) {
		t.Fatal("edge received local origin")
	}
	observation := runtimewire.Observation{RouteID: route, AttachmentGeneration: a.AttachmentGeneration, SessionID: a.Binding.SessionID, Ready: true}
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); err != nil {
		t.Fatal(err)
	}
	ready(true)
	// A repeated unchanged observation is read, not written: no row version changes.
	versions := func() (routeVersion, attachmentVersion string) {
		if err := database.SQL().QueryRowContext(ctx, `SELECT (SELECT xmin::text FROM control_routes WHERE id=$1),(SELECT xmin::text FROM runtime_carrier_attachments WHERE route_id=$1 AND edge_node_id=$2)`, route, node).Scan(&routeVersion, &attachmentVersion); err != nil {
			t.Fatal(err)
		}
		return routeVersion, attachmentVersion
	}
	routeBefore, attachmentBefore := versions()
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); err != nil {
		t.Fatal(err)
	}
	if routeAfter, attachmentAfter := versions(); routeAfter != routeBefore || attachmentAfter != attachmentBefore {
		t.Fatalf("unchanged observation rewrote rows: route %s->%s attachment %s->%s", routeBefore, routeAfter, attachmentBefore, attachmentAfter)
	}
	// A route recreated by an update starts unapplied while its carrier is already ready:
	// the next report applies it although nothing about the attachment changed.
	if _, err := database.SQL().ExecContext(ctx, `UPDATE control_routes SET applied_node_id=NULL,applied_revision=0,applied_generation=0 WHERE id=$1`, route); err != nil {
		t.Fatal(err)
	}
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); err != nil {
		t.Fatal(err)
	}
	var reapplied string
	if err := database.SQL().QueryRowContext(ctx, `SELECT coalesce(applied_node_id,'') FROM control_routes WHERE id=$1`, route).Scan(&reapplied); err != nil || reapplied != node {
		t.Fatalf("an unapplied route with a ready carrier was not applied: node=%q err=%v", reapplied, err)
	}
	if got := dnsAddresses(); len(got) != 1 || got[node] != "192.0.2.10" {
		t.Fatalf("DNS after the first carrier is ready = %v, want only %s", got, node)
	}
	backupEdge := previewattachment.PreviewEdgeIdentity{NodeID: backupNode, ProcessEpoch: "epoch_backup"}
	backupAdmissions, err := service.Pull(ctx, backupEdge, nil)
	if err != nil || len(backupAdmissions) != 1 || backupAdmissions[0].Binding.EdgeNodeID != backupNode {
		t.Fatalf("backup pull count=%d error=%v", len(backupAdmissions), err)
	}
	backupObservation := runtimewire.Observation{RouteID: route, AttachmentGeneration: a.Backup.AttachmentGeneration, SessionID: a.Backup.Binding.SessionID, Ready: true}
	if err = service.Observe(ctx, backupEdge, []runtimewire.Observation{backupObservation}); err != nil {
		t.Fatal(err)
	}
	// Both edges serve: the second carrier joins the DNS record without moving the primary route.
	if got := dnsAddresses(); len(got) != 2 || got[node] != "192.0.2.10" || got[backupNode] != "192.0.2.11" {
		t.Fatalf("DNS with both carriers ready = %v, want %s and %s", got, node, backupNode)
	}
	var appliedNode string
	if err = database.SQL().QueryRowContext(ctx, `SELECT applied_node_id FROM control_routes WHERE id=$1`, route).Scan(&appliedNode); err != nil || appliedNode != node {
		t.Fatalf("backup observation moved active route to %q: %v", appliedNode, err)
	}
	if _, err = database.Queries().GetActiveHelperRouteForEnvironment(ctx, env); err != nil {
		t.Fatalf("browser route absent after real edge observation: %v", err)
	}
	legacyRows, err := database.Queries().ListControlRoutesForNode(ctx, sql.NullString{String: node, Valid: true})
	if err != nil || len(legacyRows) != 0 {
		t.Fatal("runtime route leaked through legacy edge projection")
	}
	var previewSessionID string
	if err := database.SQL().QueryRowContext(ctx, `SELECT preview_session_id FROM runtime_carrier_attachments WHERE route_id=$1 AND edge_node_id=$2`, route, node).Scan(&previewSessionID); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{a.Binding.SessionID, previewSessionID} {
		assertCanonicalCarrierSession(t, value)
	}
	if a.Binding.SessionID == previewSessionID || a.Binding.TunnelID != route || a.Binding.ConnectorID != machine {
		t.Fatal("carrier identity lost route/principal/receiver isolation")
	}
	renewed, err := service.Attach(ctx, proof, request)
	if err != nil || renewed.Binding.SessionID != a.Binding.SessionID {
		t.Fatal("renewal changed active carrier")
	}
	ready(true)
	observation.Ready = false
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); err != nil {
		t.Fatal(err)
	}
	var primaryReady bool
	if err := database.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_valid($1,$2,$3,true)`, route, edge.NodeID, edge.ProcessEpoch).Scan(&primaryReady); err != nil || primaryReady {
		t.Fatalf("disconnected primary ready=%v error=%v", primaryReady, err)
	}
	// The independently observed standby still provides route readiness.
	ready(true)
	observation.Ready = true
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE runtime_carrier_attachments SET observed_at=now()-interval '31 seconds' WHERE route_id=$1`, route)
	ready(false)
	if _, err = database.Queries().GetActiveHelperRouteForEnvironment(ctx, env); err == nil {
		t.Fatal("stale browser route remained ready")
	}
	cross := proof
	cross.UserID = "other_account"
	if _, err = service.Attach(ctx, cross, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("cross-account attachment accepted")
	}
	stale := request
	stale.WorkerGeneration = 2
	if _, err = service.Attach(ctx, proof, stale); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unobserved worker accepted")
	}
	stale = request
	stale.ProcessID = "other_process"
	if _, err = service.Attach(ctx, proof, stale); !errors.Is(err, ErrStale) {
		t.Fatal("same-worker process replacement accepted")
	}
	exec(`UPDATE control_tunnel_nodes SET process_epoch='epoch_replacement' WHERE id=$1`, node)
	ready(false)
	replacement, err := service.Attach(ctx, proof, request)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.AttachmentGeneration <= a.AttachmentGeneration || replacement.Binding.SessionID == a.Binding.SessionID {
		t.Fatal("edge replacement did not fence carrier")
	}
	var reboundPreviewSessionID string
	if err := database.SQL().QueryRowContext(ctx, `SELECT preview_session_id FROM runtime_carrier_attachments WHERE route_id=$1 AND edge_node_id=$2`, route, node).Scan(&reboundPreviewSessionID); err != nil {
		t.Fatal(err)
	}
	if reboundPreviewSessionID != previewSessionID {
		t.Fatal("same worker edge epoch renewal replaced shared preview session")
	}
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); !errors.Is(err, ErrStale) {
		t.Fatal("old edge observation accepted")
	}
	edge.ProcessEpoch = "epoch_replacement"
	observation.AttachmentGeneration = replacement.AttachmentGeneration
	observation.SessionID = replacement.Binding.SessionID
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); err != nil {
		t.Fatal(err)
	}
	ready(true)
	if err = service.Observe(ctx, backupEdge, []runtimewire.Observation{backupObservation}); err != nil {
		t.Fatal(err)
	}
	request.ActiveEdgeNodeID, request.ActiveEdgeProcessEpoch = backupNode, "epoch_backup"
	switched, err := service.Attach(ctx, proof, request)
	if err != nil || switched.ActiveEdgeNodeID != backupNode || switched.ActiveEdgeProcessEpoch != "epoch_backup" {
		t.Fatalf("backup activation selected %q/%q: %v", switched.ActiveEdgeNodeID, switched.ActiveEdgeProcessEpoch, err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT applied_node_id FROM control_routes WHERE id=$1`, route).Scan(&appliedNode); err != nil || appliedNode != backupNode {
		t.Fatalf("active route after promotion=%q error=%v", appliedNode, err)
	}
	if _, err = database.Queries().GetActiveHelperRouteForEnvironment(ctx, env); err != nil {
		t.Fatalf("promoted browser route is unavailable: %v", err)
	}
	if got := dnsAddresses(); got[backupNode] != "192.0.2.11" {
		t.Fatalf("promoted edge missing from DNS: %v", got)
	}
	observation.Ready = false
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); err != nil {
		t.Fatal(err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT applied_node_id FROM control_routes WHERE id=$1`, route).Scan(&appliedNode); err != nil || appliedNode != backupNode {
		t.Fatalf("late old-edge observation moved route to %q: %v", appliedNode, err)
	}
	ready(true)
	// Keep registry liveness independent of the admission-expiry scenario.
	exec(`UPDATE control_tunnel_nodes SET last_heartbeat_at=now(),capacity_observed_at=now() WHERE id IN ($1,$2)`, node, backupNode)
	exec(`UPDATE runtime_carrier_attachments SET expires_at=now()-interval '1 second' WHERE route_id=$1`, route)
	ready(true)
	if got := dnsAddresses(); len(got) == 0 {
		t.Fatal("admitted runtime edge lost DNS on credential expiry")
	}
	if err = service.Observe(ctx, backupEdge, []runtimewire.Observation{backupObservation}); err != nil {
		t.Fatal("admitted runtime edge heartbeat rejected after credential expiry")
	}
	values, err := service.Pull(ctx, backupEdge, nil)
	if err != nil || len(values) == 0 || !values[0].ExpiresAt.After(time.Now()) {
		t.Fatal("current carrier did not receive fresh admission metadata")
	}

	if _, err = service.Attach(ctx, proof, request); err != nil {
		t.Fatal(err)
	}
	if err = service.Observe(ctx, backupEdge, []runtimewire.Observation{backupObservation}); err != nil {
		t.Fatal(err)
	}
	ready(true)
	exec(`UPDATE user_machines SET public_identity_key=$2 WHERE id=$1`, machine, base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	ready(false)
	exec(`UPDATE user_machines SET public_identity_key=$2 WHERE id=$1`, machine, key)
	exec(`UPDATE user_machines SET revoked_at=now() WHERE id=$1`, machine)
	ready(false)
	if got := dnsAddresses(); len(got) != 0 {
		t.Fatalf("revoked runtime edge remained advertised in DNS: %v", got)
	}
	admission, err = service.Pull(ctx, edge, nil)
	if err != nil || len(admission) != 0 {
		t.Fatal("revoked machine remains in edge snapshot")
	}
	observation.Ready = true
	if err = service.Observe(ctx, edge, []runtimewire.Observation{observation}); !errors.Is(err, ErrStale) {
		t.Fatal("revoked machine became ready")
	}
	// A current machine report changes the server's chosen edge, while a revoked
	// machine cannot report a placement or renew a carrier.
	request.EdgePreference = &runtimewire.EdgePreference{PrimaryNodeID: backupNode, PrimaryProcessEpoch: "epoch_backup", SecondaryNodeID: node, SecondaryProcessEpoch: "epoch_replacement"}
	if _, err = service.Attach(ctx, proof, request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("revoked machine changed edge placement")
	}
	exec(`UPDATE user_machines SET revoked_at=NULL WHERE id=$1`, machine)
	// Start the placement phase with live edge heartbeats; earlier scenarios
	// may take longer than the registry lease over a remote database link.
	exec(`UPDATE control_tunnel_nodes SET last_heartbeat_at=now(),capacity_observed_at=now() WHERE id IN ($1,$2)`, node, backupNode)
	preferred, err := service.Attach(ctx, proof, request)
	if err != nil {
		t.Fatal(err)
	}
	if preferred.Binding.EdgeNodeID != backupNode || preferred.Binding.EdgeProcessEpoch != "epoch_backup" {
		t.Fatalf("measured preference selected %q/%q", preferred.Binding.EdgeNodeID, preferred.Binding.EdgeProcessEpoch)
	}
	var primary, secondary string
	if err := database.SQL().QueryRowContext(ctx, `SELECT primary_node_id,secondary_node_id FROM machine_edge_placements WHERE machine_id=$1`, machine).Scan(&primary, &secondary); err != nil || primary != backupNode || secondary != node {
		t.Fatalf("stored placement primary=%q secondary=%q error=%v", primary, secondary, err)
	}
	// Capacity only stops new selection; it must not retire a valid carrier
	// before a replacement is ready.
	exec(`UPDATE control_tunnel_nodes SET capacity_used=capacity_limit WHERE id=$1`, backupNode)
	capacityRequest := request
	capacityRequest.EdgePreference = nil
	retained, err := service.Attach(ctx, proof, capacityRequest)
	if err != nil || retained.Binding.EdgeNodeID != node || retained.ActiveEdgeNodeID != backupNode {
		t.Fatalf("capacity replaced working carrier: primary=%q active=%q err=%v", retained.Binding.EdgeNodeID, retained.ActiveEdgeNodeID, err)
	}
	exec(`UPDATE control_tunnel_nodes SET capacity_used=0 WHERE id=$1`, backupNode)
	// A managed runtime hostname cannot use a customer's edge, even when that
	// edge is allowed by the mixed tunnel pool and was previously active.
	exec(`UPDATE control_tunnel_nodes SET tls_spki_sha256=$2 WHERE id=$1`, backupNode, []byte(public))
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'fixture','runtime edge','tunnel','account',$4,$1,true)`, prefix+"_installation", backupNode, []byte(public), account)
	ready(false)
	if got := dnsAddresses(); len(got) != 0 {
		t.Fatalf("customer edge remained advertised for managed runtime hostname: %v", got)
	}
	if admissions, err := service.Pull(ctx, backupEdge, nil); err != nil || len(admissions) != 0 {
		t.Fatalf("customer edge retained runtime authority: count=%d err=%v", len(admissions), err)
	}
	managed, err := service.Attach(ctx, proof, request)
	if err != nil || managed.Binding.EdgeNodeID != node || managed.Backup != nil || managed.ActiveEdgeNodeID != node {
		t.Fatalf("managed runtime placement: primary=%q active=%q backup=%v err=%v", managed.Binding.EdgeNodeID, managed.ActiveEdgeNodeID, managed.Backup != nil, err)
	}
	for _, candidate := range managed.EdgeCandidates {
		if candidate.NodeID == backupNode {
			t.Fatal("customer edge offered as managed runtime candidate")
		}
	}
	exec(`INSERT INTO selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2]::text[])`, account, prefix+"_installation")
	exec(`UPDATE control_tunnel_nodes SET endpoint_host='backup.example.test' WHERE id=$1`, backupNode)
	customer, err := service.Attach(ctx, proof, request)
	if err != nil || customer.Binding.EdgeNodeID != backupNode || customer.Backup != nil || customer.Hostname == machine+".runtime.example.test" || !strings.HasSuffix(customer.Hostname, ".backup.example.test") {
		t.Fatalf("customer runtime did not use its owned edge and routing alias: %#v err=%v", customer, err)
	}
	for _, candidate := range customer.EdgeCandidates {
		if candidate.NodeID != backupNode {
			t.Fatal("managed candidate escaped customer-only workspace")
		}
	}
	if _, err := IngressURL(ctx, database, env, customer.Hostname); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unready customer carrier advertised: %v", err)
	}
	if err := service.Observe(ctx, backupEdge, []runtimewire.Observation{{RouteID: route, AttachmentGeneration: customer.AttachmentGeneration, SessionID: customer.Binding.SessionID, Ready: true}}); err != nil {
		t.Fatal(err)
	}
	if actual, err := IngressURL(ctx, database, env, customer.Hostname); err != nil || actual != "https://backup.example.test/v1/runtime/"+customer.Hostname {
		t.Fatalf("customer infrastructure URL=%q error=%v", actual, err)
	}
	exec(`UPDATE control_tunnel_nodes SET ready=false WHERE id=$1`, backupNode)
	if _, err := service.Attach(ctx, proof, request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("customer outage used managed fallback: %v", err)
	}
	exec(`UPDATE control_tunnel_nodes SET ready=true WHERE id=$1`, backupNode)
	exec(`UPDATE selfhost_pool_policies SET mode='managed-only',installation_ids='{}' WHERE scope_kind='account' AND scope_id=$1 AND capability='tunnel'`, account)
	restored, err := service.Attach(ctx, proof, request)
	if err != nil || restored.Binding.EdgeNodeID != node || restored.Hostname != machine+".runtime.example.test" {
		t.Fatalf("managed runtime restoration=%#v error=%v", restored, err)
	}
}

func assertCanonicalCarrierSession(t *testing.T, value string) {
	t.Helper()
	suffix, ok := strings.CutPrefix(value, "session_")
	id, err := uuid.Parse(suffix)
	if !ok || err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != suffix {
		t.Fatalf("invalid carrier session %q", value)
	}
}
func TestCarrierSessionEntropyFailure(t *testing.T) {
	if id, err := newCarrierSessionID(strings.NewReader("")); id != "" || !errors.Is(err, io.EOF) {
		t.Fatalf("entropy failure id=%q err=%v", id, err)
	}
}
