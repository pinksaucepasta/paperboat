package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountprefs"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/testauth"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
)

// sendPrepareFixture is the isolated PostgreSQL world one prepare test runs
// in: a router with device auth, machines, team inbox and account
// preferences, plus control-plane routes so descriptors mint without a live
// runtime. Every test builds its own fixture; resetIntegrationTables (via
// newSendPrepareRouter) wipes the isolated database first, like the auth
// integration router.
type sendPrepareFixture struct {
	t         *testing.T
	ctx       context.Context
	store     *db.DB
	router    http.Handler
	inbox     *teaminbox.Service
	authority *teams.Service
	suffix    string
}

func newSendPrepareFixture(t *testing.T) *sendPrepareFixture {
	t.Helper()
	dsn := postgresDSN(t)
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	resetIntegrationTables(t, store)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	auditWriter := audit.NewWriter(store)
	service := auth.NewService(store, auditWriter, testauth.FakeWorkOSVerifier{}, []string{"test-session-key"}, true, "https://login.example.test")
	machineService := auth.NewDeviceService(store, auditWriter, config.Default().CLIAuth, []string{"test-machine-hash-key"})
	machineService.ConfigureAuthorizationEncryptionKey("test-enrollment-key")
	billingService := billing.NewWorkspaceService(store, nil, "test-billing-key")
	if err := billingService.InitializeAllowancePolicy(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	userMachineService := usermachines.New(store, auditWriter, usermachines.Policy{}, billingService)
	userMachineService.ConfigureInstallationEncryption("test-enrollment-key")
	machineService.SetEnroller(usermachines.DeviceEnroller{Service: userMachineService})
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	userMachineService.ConfigureMachineControl(nil, "https://prepare.invalid.test")
	userMachineService.ConfigureTerminalSessions(signer, nil)
	userMachineService.ConfigureFileTransfer(terminalprotocol.FileTransferPolicy{Revision: "send-prepare-test", MaxBatchFiles: 10, AutomaticMaxBatchBytes: 2_000_000_000, MaxConcurrentTransfers: 2, RetentionSeconds: 604800, DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 2_000_000_000})
	inbox, err := teaminbox.New(store, config.Default().Access.FileTransfer)
	if err != nil {
		t.Fatal(err)
	}
	userMachineService.ConfigureTeamInbox(inbox)
	authority := teams.NewService(store)
	cfg := config.Default()
	router := authTestOriginHandler{next: NewRouter(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return nil
		}),
		Auth:               service,
		DeviceAuth:         machineService,
		AccountPreferences: accountprefs.New(store),
		WorkspaceBilling:   billingService,
		Workspaces:         workspace.New(store),
		Machines:           userMachineService,
		Teams:              authority,
		TeamInbox:          inbox,
	})}
	return &sendPrepareFixture{t: t, ctx: ctx, store: store, router: router, inbox: inbox, authority: authority, suffix: fmt.Sprint(time.Now().UnixNano())}
}

func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run send prepare integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func (f *sendPrepareFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// actor signs a browser login up and returns the account ID with a device
// bearer token, the same CLI credential shape the descriptor route takes.
func (f *sendPrepareFixture) actor(name string) (account, token string) {
	f.t.Helper()
	email := "prepare-" + name + "-" + f.suffix + "@invalid.test"
	cookies := loginCookies(f.t, f.router, "prepare_"+name+"_"+f.suffix+":"+email+":"+name)
	account = userIDByEmail(f.t, f.store, email)
	return account, authorizeCLI(f.t, f.router, cookies).AccessToken
}

// machine inserts an owned machine; fileReceive adds the receive capability
// the descriptor requires, online toggles its reachability. The carrier join
// (runtime_carrier_current) needs the same identity key and worker generation
// on both sides, so the key is deterministic per machine ID.
func (f *sendPrepareFixture) machine(id, owner, alias string, fileReceive, online bool) {
	f.t.Helper()
	caps := `ARRAY[]::text[]`
	if fileReceive {
		caps = `ARRAY['terminal_host','file_receive']`
	}
	state, seatOnline := "offline", false
	if online {
		state, seatOnline = "online", true
	}
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation,worker_generation,public_identity_key,last_seen_at,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,$4,'linux','amd64','/workspace',$5,'occupied',$6,1,1,$7,now(),`+caps+`,`+caps+`)`,
		id, owner, "env_"+id, alias, state, seatOnline, "prepare-key-"+id)
	f.exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active') ON CONFLICT DO NOTHING`, "env_"+id, id, owner)
}

// route wires the destination environment's control-plane rows so the
// descriptor mints without a live runtime. Every field the
// runtime_carrier_current join checks is kept equal: tunnel process_epoch ==
// carrier edge_process_epoch, machine public_identity_key == carrier
// public_identity_key, machine worker/installation generations == carrier's,
// route revisions == 1, heartbeats fresh.
func (f *sendPrepareFixture) route(env, machineID string) {
	f.t.Helper()
	edge := "edge_" + env
	epoch := "epoch_" + env
	key := "prepare-key-" + machineID
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now()) ON CONFLICT DO NOTHING`, edge, epoch)
	f.exec(`INSERT INTO paperboat.control_connector_generations(environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES($1,$2,1,'development',$3,'admitted') ON CONFLICT DO NOTHING`, env, machineID, edge)
	f.exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES($1,$2,'runtime_https_wss','workspace.invalid.test','127.0.0.1',8080,1,1,$3,1) ON CONFLICT DO NOTHING`, "route_"+env, env, edge)
	var owner string
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT user_id FROM paperboat.user_machines WHERE id=$1`, machineID).Scan(&owner); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true) ON CONFLICT DO NOTHING`,
		"route_"+env, machineID, owner, "process_"+env, edge, epoch, key, "session_"+uuid.NewString(), "session_"+uuid.NewString())
	var ready bool
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, "route_"+env).Scan(&ready); err != nil || !ready {
		f.t.Fatalf("prepare route not current: ready=%v err=%v", ready, err)
	}
}

// team shares destMachine (owned by owner) with member for file sends, like
// the stored team-inbox fixture, and returns the team ID.
func (f *sendPrepareFixture) team(teamID, owner, member, destMachine string) {
	f.t.Helper()
	current, err := f.authority.Create(f.ctx, owner, teams.CreateRequest{OperationID: "create_" + f.suffix, TeamID: teamID})
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,now()-interval '1 day',now()+interval '30 days')`, "team:"+teamID, teamID)
	current, err = f.authority.Get(f.ctx, owner, teamID)
	if err != nil {
		f.t.Fatal(err)
	}
	invite, err := f.authority.Invite(f.ctx, owner, teamID, teams.InviteRequest{OperationID: "invite_" + f.suffix, ExpectedGeneration: current.Generation, AccountID: member})
	if err != nil {
		f.t.Fatal(err)
	}
	current, err = f.authority.Accept(f.ctx, member, invite.InvitationID, teams.AcceptRequest{OperationID: "accept_" + f.suffix})
	if err != nil {
		f.t.Fatal(err)
	}
	current, err = f.authority.Machine(f.ctx, owner, teamID, teams.MachineRequest{OperationID: "share_" + f.suffix, ExpectedGeneration: current.Generation, MachineID: destMachine, Action: "share"})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.authority.GrantMachine(f.ctx, owner, teamID, teams.MachineGrantRequest{OperationID: "grant_" + f.suffix, ExpectedGeneration: current.Generation, MachineID: destMachine, Audience: "selected_member", AccountID: member, Capabilities: []string{"files"}, Active: true}); err != nil {
		f.t.Fatal(err)
	}
}

// prepareFile returns the manifest's single test file with a real digest.
func prepareFile() teaminbox.File {
	digest := sha256.Sum256([]byte("prepare"))
	return teaminbox.File{Basename: "a.bin", Size: 12, SHA256: hex.EncodeToString(digest[:])}
}

type sendPrepareCall struct {
	DestinationID string           `json:"destination_machine_id"`
	SourceMachine string           `json:"source_machine_id"`
	SessionID     string           `json:"session_id"`
	RequestID     string           `json:"request_id"`
	OperationID   string           `json:"operation_id"`
	BatchID       string           `json:"batch_id"`
	Files         []teaminbox.File `json:"files"`
	ExpiresAt     time.Time        `json:"expires_at"`
	TransferMode  string           `json:"transfer_mode"`
	DeclaredBytes int64            `json:"declared_bytes"`
}

type sendPrepareResult struct {
	Status           int
	ErrorCode        string
	MachineID        string
	MachineAlias     string
	Online           bool
	RequestID        string
	RequestStatus    string
	Stored           bool
	TeamID           string
	ManifestDigest   string
	FileTransferMode string
	Descriptor       bool
	Endpoint         string
	Details          map[string]any
}

func (f *sendPrepareFixture) post(token, path string, body any) sendPrepareResult {
	f.t.Helper()
	var out sendPrepareResult
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	out.Status = rec.Code
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		f.t.Fatalf("invalid API response: %d %s", rec.Code, rec.Body.String())
	}
	if envelope.Error != nil {
		out.ErrorCode = envelope.Error.Code
		out.Details = envelope.Error.Details
		return out
	}
	var data struct {
		Machine struct {
			ID      string `json:"id"`
			Alias   string `json:"alias"`
			Online  bool   `json:"online"`
			EnvID   string `json:"environment_id"`
			Install int64  `json:"installation_generation"`
		} `json:"machine"`
		Request struct {
			RequestID      string `json:"request_id"`
			Status         string `json:"status"`
			Stored         bool   `json:"stored"`
			TeamID         string `json:"team_id"`
			ManifestDigest string `json:"manifest_digest"`
		} `json:"request"`
		FileTransferMode string `json:"file_transfer_mode"`
		Descriptor       *struct {
			Endpoint string `json:"endpoint"`
		} `json:"descriptor"`
		// The legacy descriptor route answers the bare descriptor.
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		f.t.Fatalf("invalid prepare data: %s", envelope.Data)
	}
	out.MachineID, out.MachineAlias, out.Online = data.Machine.ID, data.Machine.Alias, data.Machine.Online
	out.RequestID, out.RequestStatus, out.Stored, out.TeamID, out.ManifestDigest = data.Request.RequestID, data.Request.Status, data.Request.Stored, data.Request.TeamID, data.Request.ManifestDigest
	out.FileTransferMode = data.FileTransferMode
	if data.Descriptor != nil {
		out.Descriptor, out.Endpoint = true, data.Descriptor.Endpoint
	}
	// The legacy descriptor route answers the bare descriptor (no machine/request envelope).
	if data.Endpoint != "" {
		out.Descriptor, out.Endpoint = true, data.Endpoint
	}
	return out
}

func (f *sendPrepareFixture) baseCall(destRef, source string) sendPrepareCall {
	return sendPrepareCall{
		DestinationID: destRef, SourceMachine: source,
		RequestID: "request_" + f.suffix, OperationID: "operation_" + f.suffix, BatchID: "batch_" + f.suffix,
		Files: []teaminbox.File{prepareFile()}, ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
		TransferMode: "automatic", DeclaredBytes: 12,
	}
}

func TestSendPrepareOwnMachineReturnsDescriptorInOneCallPostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	sender, token := f.actor("own")
	f.machine("prepare_source_"+f.suffix, sender, "src", false, true)
	dest := "prepare_dest_" + f.suffix
	f.machine(dest, sender, "hn", true, true)
	f.route("env_"+dest, dest)

	got := f.post(token, "/v1/sends/prepare", f.baseCall(dest, "prepare_source_"+f.suffix))
	if got.Status != http.StatusCreated {
		t.Fatalf("prepare status=%d code=%s", got.Status, got.ErrorCode)
	}
	if got.RequestStatus != "not_required" || got.Stored {
		t.Fatalf("own-machine request status=%q stored=%v", got.RequestStatus, got.Stored)
	}
	if got.FileTransferMode != "automatic" {
		t.Fatalf("file_transfer_mode=%q", got.FileTransferMode)
	}
	if !got.Descriptor || !strings.HasPrefix(got.Endpoint, "https://") || !strings.HasSuffix(got.Endpoint, "/v1/file-transfers") {
		t.Fatalf("descriptor missing or endpoint=%q", got.Endpoint)
	}
}

func TestSendPrepareTeamApprovedMintsDescriptorPostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	owner, _ := f.actor("owner")
	member, memberToken := f.actor("member")
	f.machine("prepare_member_source_"+f.suffix, member, "member-src", false, true)
	dest := "prepare_team_dest_" + f.suffix
	f.machine(dest, owner, "teamhn", true, true)
	f.route("env_"+dest, dest)
	teamID := "prepare-team-" + f.suffix
	f.team(teamID, owner, member, dest)
	policy, err := f.inbox.Policy(f.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.inbox.SetPolicy(f.ctx, owner, teaminbox.Policy{Acceptance: "automatic"}, policy.Generation); err != nil {
		t.Fatal(err)
	}

	// transfer_mode:"" reads the sender's account preference (automatic by default).
	call := f.baseCall(dest, "prepare_member_source_"+f.suffix)
	call.TransferMode = ""
	got := f.post(memberToken, "/v1/sends/prepare?workspace="+teamID, call)
	if got.Status != http.StatusCreated {
		t.Fatalf("prepare status=%d code=%s", got.Status, got.ErrorCode)
	}
	if got.RequestStatus != "approved" || got.TeamID != teamID {
		t.Fatalf("team request status=%q team=%q", got.RequestStatus, got.TeamID)
	}
	if got.FileTransferMode != "automatic" {
		t.Fatalf("empty mode did not default to the account preference: %q", got.FileTransferMode)
	}
	if !got.Descriptor || got.Endpoint == "" {
		t.Fatal("approved team send returned no inline descriptor")
	}
}

func TestSendPreparePendingPollsThenDescriptorPostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	owner, _ := f.actor("owner")
	member, memberToken := f.actor("member")
	f.machine("prepare_pending_source_"+f.suffix, member, "pending-src", false, true)
	dest := "prepare_pending_dest_" + f.suffix
	f.machine(dest, owner, "pendinghn", true, true)
	f.route("env_"+dest, dest)
	pendingTeam := "prepare-pending-team-" + f.suffix
	f.team(pendingTeam, owner, member, dest)

	got := f.post(memberToken, "/v1/sends/prepare?workspace="+pendingTeam, f.baseCall(dest, "prepare_pending_source_"+f.suffix))
	if got.Status != http.StatusAccepted {
		t.Fatalf("pending prepare status=%d code=%s", got.Status, got.ErrorCode)
	}
	if got.RequestStatus != "pending" || got.Descriptor {
		t.Fatalf("pending prepare must omit the descriptor: status=%q descriptor=%v", got.RequestStatus, got.Descriptor)
	}

	// The existing poll route still reports the request while approval is outstanding.
	req := httptest.NewRequest(http.MethodGet, "/v1/team-inbox/requests/"+got.RequestID, nil)
	req.Header.Set("Authorization", "Bearer "+memberToken)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body.String())
	}

	// After approval the existing descriptor route mints the credential.
	request, err := f.inbox.Get(f.ctx, member, got.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.inbox.Decide(f.ctx, owner, got.RequestID, "approved", request.DecisionGeneration); err != nil {
		t.Fatal(err)
	}
	desc := f.post(memberToken, "/v1/machines/"+dest+"/file-transfer-descriptor?workspace="+pendingTeam, map[string]any{
		"source_machine_id": "prepare_pending_source_" + f.suffix,
		"request_id":        got.RequestID,
		"manifest_digest":   got.ManifestDigest,
		"transfer_mode":     "automatic",
		"declared_bytes":    12,
	})
	if desc.Status != http.StatusOK || !desc.Descriptor {
		t.Fatalf("descriptor after approval status=%d code=%s", desc.Status, desc.ErrorCode)
	}
}

func TestSendPrepareOfflineDestinationPostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	sender, token := f.actor("sender")
	f.machine("prepare_offline_source_"+f.suffix, sender, "offsrc", false, true)
	dest := "prepare_offline_dest_" + f.suffix
	f.machine(dest, sender, "offhn", true, false)

	got := f.post(token, "/v1/sends/prepare", f.baseCall(dest, "prepare_offline_source_"+f.suffix))
	if got.Status != http.StatusConflict || got.ErrorCode != "machine_offline" {
		t.Fatalf("offline prepare status=%d code=%s", got.Status, got.ErrorCode)
	}
}

func TestSendPrepareUnknownDestinationIsRefusedPostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	_, token := f.actor("sender")
	sender, _ := f.actor("other")
	f.machine("prepare_unknown_source_"+f.suffix, sender, "unknownsrc", false, true)
	// A machine ID the account neither owns nor was granted: the same refusal the inbox gives, no descriptor.
	got := f.post(token, "/v1/sends/prepare", f.baseCall("machine_does_not_exist", "prepare_unknown_source_"+f.suffix))
	if got.Status != http.StatusForbidden && got.Status != http.StatusNotFound {
		t.Fatalf("unknown destination status=%d code=%s", got.Status, got.ErrorCode)
	}
	if got.Descriptor {
		t.Fatal("a refused prepare returned a descriptor")
	}
}

func TestSendPrepareEmptyModeUsesAccountPreferencePostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	sender, token := f.actor("pref")
	f.machine("prepare_pref_source_"+f.suffix, sender, "prefsrc", false, true)
	dest := "prepare_pref_dest_" + f.suffix
	f.machine(dest, sender, "prefhn", true, true)
	f.route("env_"+dest, dest)
	unlimited := "unlimited"
	prefs := accountprefs.New(f.store)
	if _, err := prefs.Set(f.ctx, sender, accountprefs.Update{FileTransferMode: &unlimited}); err != nil {
		t.Fatal(err)
	}

	call := f.baseCall(dest, "prepare_pref_source_"+f.suffix)
	call.TransferMode = ""
	got := f.post(token, "/v1/sends/prepare", call)
	if got.Status != http.StatusCreated {
		t.Fatalf("prepare status=%d code=%s", got.Status, got.ErrorCode)
	}
	if got.FileTransferMode != "unlimited" || !got.Descriptor {
		t.Fatalf("empty mode must use the account preference: mode=%q descriptor=%v", got.FileTransferMode, got.Descriptor)
	}
}

func TestSendPrepareTransferTooLargePostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	sender, token := f.actor("large")
	f.machine("prepare_large_source_"+f.suffix, sender, "largesrc", false, true)
	dest := "prepare_large_dest_" + f.suffix
	f.machine(dest, sender, "largehn", true, true)
	f.route("env_"+dest, dest)

	call := f.baseCall(dest, "prepare_large_source_"+f.suffix)
	call.DeclaredBytes = 2_000_000_001
	got := f.post(token, "/v1/sends/prepare", call)
	if got.Status != http.StatusConflict || got.ErrorCode != "transfer_too_large" {
		t.Fatalf("too-large prepare status=%d code=%s", got.Status, got.ErrorCode)
	}
}

func TestSendPrepareRefusesUnknownTransferModePostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	sender, token := f.actor("badmode")
	f.machine("prepare_badmode_source_"+f.suffix, sender, "badsrc", false, true)
	dest := "prepare_badmode_dest_" + f.suffix
	f.machine(dest, sender, "badhn", true, true)
	f.route("env_"+dest, dest)

	call := f.baseCall(dest, "prepare_badmode_source_"+f.suffix)
	call.TransferMode = "garbage"
	got := f.post(token, "/v1/sends/prepare", call)
	if got.Status != http.StatusBadRequest || got.ErrorCode != "invalid_transfer_intent" {
		t.Fatalf("garbage mode prepare status=%d code=%s", got.Status, got.ErrorCode)
	}
}

func TestSendPrepareIdempotentRetryPostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	owner, _ := f.actor("idemowner")
	member, memberToken := f.actor("idemmember")
	f.machine("prepare_idem_source_"+f.suffix, member, "idemsrc", false, true)
	dest := "prepare_idem_dest_" + f.suffix
	f.machine(dest, owner, "idemhn", true, true)
	f.route("env_"+dest, dest)
	teamID := "prepare-idem-team-" + f.suffix
	f.team(teamID, owner, member, dest)
	policy, err := f.inbox.Policy(f.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.inbox.SetPolicy(f.ctx, owner, teaminbox.Policy{Acceptance: "automatic"}, policy.Generation); err != nil {
		t.Fatal(err)
	}

	call := f.baseCall(dest, "prepare_idem_source_"+f.suffix)
	first := f.post(memberToken, "/v1/sends/prepare?workspace="+teamID, call)
	second := f.post(memberToken, "/v1/sends/prepare?workspace="+teamID, call)
	if first.Status != http.StatusCreated || second.Status != http.StatusCreated {
		t.Fatalf("retry statuses=%d,%d codes=%s,%s", first.Status, second.Status, first.ErrorCode, second.ErrorCode)
	}
	if first.RequestID != second.RequestID || first.RequestStatus != "approved" || second.RequestStatus != "consumed" {
		t.Fatalf("idempotent retry changed the request: %q/%q status %q/%q", first.RequestID, second.RequestID, first.RequestStatus, second.RequestStatus)
	}
	// The first call consumed the approval; the retry names the same request and re-mints the descriptor.
	if !first.Descriptor || !second.Descriptor {
		t.Fatal("idempotent retry must re-mint the descriptor")
	}

	// Same operation_id with different content conflicts, like the inbox route.
	call.BatchID = "other-batch-" + f.suffix
	conflict := f.post(memberToken, "/v1/sends/prepare?workspace="+teamID, call)
	if conflict.Status != http.StatusConflict || conflict.ErrorCode != "team_inbox_conflict" {
		t.Fatalf("conflicting retry status=%d code=%s", conflict.Status, conflict.ErrorCode)
	}
}

func TestSendPrepareAuthFailuresMatchDescriptorRoutePostgres(t *testing.T) {
	f := newSendPrepareFixture(t)
	sender, token := f.actor("auth")
	f.machine("prepare_auth_source_"+f.suffix, sender, "authsrc", false, true)
	dest := "prepare_auth_dest_" + f.suffix
	f.machine(dest, sender, "authhn", true, true)
	f.route("env_"+dest, dest)
	call := f.baseCall(dest, "prepare_auth_source_"+f.suffix)

	descriptorBody := map[string]any{
		"source_machine_id": "prepare_auth_source_" + f.suffix,
		"transfer_mode":     "automatic",
		"declared_bytes":    12,
	}
	// No token: both routes answer unauthenticated.
	prepare := f.post("", "/v1/sends/prepare", call)
	descriptor := f.post("", "/v1/machines/"+dest+"/file-transfer-descriptor", descriptorBody)
	if prepare.Status != http.StatusUnauthorized || prepare.ErrorCode != "unauthenticated" {
		t.Fatalf("prepare without token status=%d code=%s", prepare.Status, prepare.ErrorCode)
	}
	if descriptor.Status != prepare.Status || descriptor.ErrorCode != prepare.ErrorCode {
		t.Fatalf("descriptor without token status=%d code=%s, want prepare's %d %s", descriptor.Status, descriptor.ErrorCode, prepare.Status, prepare.ErrorCode)
	}

	// Wrong scope: narrow the session to machines:read and compare codes.
	var session string
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT id FROM paperboat.cli_client_sessions WHERE user_id=$1 AND state='active'`, sender).Scan(&session); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE paperboat.cli_client_sessions SET scopes=ARRAY['machines:read']::text[] WHERE id=$1`, session)
	prepare = f.post(token, "/v1/sends/prepare", call)
	descriptor = f.post(token, "/v1/machines/"+dest+"/file-transfer-descriptor", descriptorBody)
	if prepare.Status != http.StatusForbidden || prepare.ErrorCode != "insufficient_scope" {
		t.Fatalf("prepare with wrong scope status=%d code=%s", prepare.Status, prepare.ErrorCode)
	}
	if descriptor.Status != prepare.Status || descriptor.ErrorCode != prepare.ErrorCode {
		t.Fatalf("descriptor with wrong scope status=%d code=%s, want prepare's %d %s", descriptor.Status, descriptor.ErrorCode, prepare.Status, prepare.ErrorCode)
	}
}

func TestSendPrepareRouteRegistration(t *testing.T) {
	// The folded inbox route is refused for both, so prepare must not widen what they can create.
	if scopedTokenRoute(httptest.NewRequest(http.MethodPost, "/v1/sends/prepare", nil)) {
		t.Fatal("a scoped token can prepare a send, but it cannot create the inbox request it folds")
	}
	if recipeRoute(httptest.NewRequest(http.MethodPost, "/v1/sends/prepare", nil), "") {
		t.Fatal("a recipe session can prepare a send, but it cannot create the inbox request it folds")
	}
}
