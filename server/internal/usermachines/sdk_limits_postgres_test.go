package usermachines

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

func TestSDKSessionRateLimits(t *testing.T) {
	f, source, _ := newWorkspaceDescriptorFixture(t)
	ctx, token, session := sdkSession(t, f, "exec")
	cfg := config.Default()
	limits := cfg.Billing.Limits
	limits.SDK = config.SDKLimits{SessionsPerToken: 2, SessionsPerAccount: 3, OperationsPerToken: 1, OperationsPerAccount: 2}
	planlimits.Configure(limits, "")
	t.Cleanup(func() { planlimits.Configure(cfg.Billing.Limits, ""); planlimits.ConfigureSDKRates(cfg.RateLimits) })
	limited := func(err error, kind string) {
		t.Helper()
		var limit *planlimits.SDKLimitError
		if !errors.As(err, &limit) || limit.Kind != kind || limit.RetryAfter <= 0 {
			t.Fatalf("expected typed %s limit, got %T", kind, err)
		}
	}
	device := auth.NewDeviceService(f.store, f.service.audit, cfg.CLIAuth, []string{"sdk-test-token-hash-key"})
	start := func(account, id string) error {
		_, err := device.StartAPITokenSession(ctx, auth.APITokenSessionRequest{TokenID: id, Account: account, Label: "sdk", DeviceType: "container", OS: "linux"})
		return err
	}
	if err := start(f.account, token); err != nil {
		t.Fatal(err)
	}
	limited(start(f.account, token), "sessions")
	other := f
	other.suffix += "_other"
	otherCtx, otherToken, otherSession := sdkSession(t, other, "exec")
	limited(start(f.account, otherToken), "sessions")
	// Concurrent descriptors contend for one persisted reservation, not an in-memory counter.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, op := range []string{"operation_sdk_limit_a", "operation_sdk_limit_b"} {
		wg.Add(1)
		go func(op string) {
			defer wg.Done()
			_, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, op, true)
			results <- err
		}(op)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			limited(err, "operations")
		}
	}
	if successes != 1 {
		t.Fatal("concurrent descriptors exceeded the token reservation limit")
	}
	for i, capability := range []string{"files", "private_access"} {
		if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.api_token_scope_rules(token_id,entry,target_kind,target_id,action,ports) VALUES($1,$2,'machine',$3,$4,'[5432,5433)')`, token, i+1, f.machine, capability); err != nil {
			t.Fatal(err)
		}
	}
	_, fileErr := f.service.FileTransferDescriptor(ctx, f.account, "", f.machine, session, "", TransferIntent{Mode: "automatic", DeclaredBytes: 1})
	limited(fileErr, "operations")
	now := time.Now().UTC()
	services := machineservices.New(f.store, "local.example.test")
	if err := services.RecordMachineServices(ctx, "env_"+f.suffix, f.machine, wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "sdk-limit-0123456789", StartedAt: now, Generation: 1, Services: []wire.Port{{Port: 5432, AddressFamily: "ipv4"}}}, now); err != nil {
		t.Fatal(err)
	}
	resolver, _ := nativeprivateaccess.NewSQLResolver(f.store)
	private := nativeprivateaccess.Service{Resolver: resolver, Signer: f.service.controlSigner, Issuer: f.service.issuer}
	_, privateErr := private.Issue(ctx, nativeprivateaccess.Request{AccountID: f.account, UserID: f.account, CLIClientSessionID: session, OperationID: "operation_sdk_limit_private", ResourceKind: "machine_service", ResourceID: f.machine, RouteID: "tcp:5432", Protocol: "tcp"})
	limited(privateErr, "operations")
	// Account quotas span different tokens; ordinary pb login remains independent.
	limits.SDK.OperationsPerToken = 2
	planlimits.Configure(limits, "")
	if _, err := f.service.ExecDescriptor(otherCtx, f.account, "", f.machine, otherSession, "operation_sdk_limit_other", true); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_limit_account", true)
	limited(err, "operations")
	// Token expiry does not release an outstanding operation reservation.
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.api_tokens SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 minute' WHERE id=$1`, token); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.ExecDescriptor(otherCtx, f.account, "", f.machine, otherSession, "operation_sdk_limit_expired_token", true)
	limited(err, "operations")
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.api_tokens SET expires_at=now()+interval '1 hour' WHERE id=$1`, token); err != nil {
		t.Fatal(err)
	}
	personal := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	if _, err := f.service.ExecDescriptor(personal, f.account, source, f.machine, "ordinary-cli", "operation_ordinary_login", true); err != nil {
		t.Fatal("SDK quota affected ordinary login")
	}
	// An operation nobody ends (a terminal, a client dropped before it connected) is released when its admission
	// expires: ~10 dropped runs once held a token's cap until the token was revoked (2026-11-12).
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_access_sessions SET expires_at=now()-interval '1 minute' WHERE cli_client_session_id=$1`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_limit_expired", true); err != nil {
		t.Fatalf("expired admissions still held the reservation: %v", err)
	}
	// A live operation still holds it until its destination outcome.
	_, err = f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_limit_live", true)
	limited(err, "operations")
	var access, operation string
	if err := f.store.SQL().QueryRowContext(ctx, `SELECT id,operation_id FROM paperboat.user_machine_access_sessions WHERE cli_client_session_id=$1 AND state='active' AND expires_at>now()`, session).Scan(&access, &operation); err != nil {
		t.Fatal(err)
	}
	end := OfflineAuditEvent{EventID: "sdk_limit_end_" + f.suffix, Kind: "sdk_operation_ended", Capability: "exec", PeerAccountID: f.account, PeerEndpointID: session, SessionID: access, OperationID: operation, Outcome: "succeeded", OccurredAt: time.Now().UTC()}
	if _, err := f.service.RecordOfflineAuditBacklog(ctx, f.machine, 1, []OfflineAuditEvent{end}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_limit_released", true); err != nil {
		t.Fatal("destination outcome did not free reservation")
	}
	t.Run("request rates", func(t *testing.T) { testSDKRequestRates(t, f) })
}

func testSDKRequestRates(t *testing.T, f workspaceMachineFixture) {
	f.suffix += "_rates"
	ctx, token, session := sdkSession(t, f, "exec")
	other := f
	other.suffix += "_rate_other"
	otherCtx, _, otherSession := sdkSession(t, other, "exec")
	cfg := config.Default()
	limited := func(err error, kind string) {
		t.Helper()
		var limit *planlimits.SDKLimitError
		if !errors.As(err, &limit) || limit.Kind != kind || limit.RetryAfter <= 0 {
			t.Fatalf("expected typed %s limit, got %T", kind, err)
		}
	}
	device := auth.NewDeviceService(f.store, f.service.audit, cfg.CLIAuth, []string{"sdk-test-token-hash-key"})
	start := func(account, id string) error {
		_, err := device.StartAPITokenSession(ctx, auth.APITokenSessionRequest{TokenID: id, Account: account, Label: "sdk", DeviceType: "container", OS: "linux"})
		return err
	}
	t.Cleanup(func() { planlimits.ConfigureSDKRates(cfg.RateLimits); planlimits.Configure(cfg.Billing.Limits, "") })
	var err error
	// Request buckets are independent by account and token and cover descriptors and session creation.
	planlimits.Configure(cfg.Billing.Limits, "")
	rates := cfg.RateLimits
	rates.SDKSessionPerMinute = 1
	rates.SDKSessionBurst = 1
	rates.SDKDescriptorPerMinute = 1
	rates.SDKDescriptorBurst = 1
	planlimits.ConfigureSDKRates(rates)
	if _, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_rate_first", true); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_rate_second", true)
	limited(err, "descriptors")
	_, err = f.service.ExecDescriptor(otherCtx, f.account, "", f.machine, otherSession, "operation_sdk_rate_other_token", true)
	limited(err, "descriptors")
	if err := start(f.account, token); err != nil {
		t.Fatal(err)
	}
	limited(start(f.account, token), "sessions")
	independent := newWorkspaceMachineFixture(t)
	independent.service.ConfigureAccess(fakeCredentialIssuer{}, f.service.issuer)
	independent.service.ConfigureTerminalSessions(f.service.controlSigner, nil)
	independentCtx, independentToken, independentSession := sdkSession(t, independent, "exec")
	if _, err := independent.service.ExecDescriptor(independentCtx, independent.account, "", independent.machine, independentSession, "operation_sdk_rate_independent", true); err != nil {
		t.Fatal("one account exhausted another account's descriptor quota")
	}
	if err := start(independent.account, independentToken); err != nil {
		t.Fatal("one account exhausted another account's session quota")
	}
}
