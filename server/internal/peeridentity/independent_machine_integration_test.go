package peeridentity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestIndependentMachineEnrollmentWithoutAccountRootPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run independent machine enrollment")
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
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	accountID, machineID, environmentID := "peer_independent_user_"+suffix, "peer_independent_machine_"+suffix, "peer_independent_env_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, accountID, "workos_"+accountID, accountID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, environmentID, "workspace_"+suffix, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation) VALUES ($1,$2,$3,'independent-host','linux','amd64','/workspace','offline','occupied',$4,3)`, machineID, accountID, environmentID, "machine_identity_public_key_"+suffix); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := signedFixture(t, private, accountID, wire.RoleMachine, machineID, 3, 1, now.Add(-time.Minute), now.Add(time.Hour))
	quicOffset := 5 + 2 + len(accountID) + 1 + 2 + len(machineID)
	copy(raw[quicOffset:quicOffset+32], public)
	payload := raw[:len(raw)-ed25519.SignatureSize]
	copy(raw[len(payload):], ed25519.Sign(private, payload))
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository)
	cliID := "peer_independent_cli_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$3,'independent-cli','cli','linux',ARRAY['machines:read'],'active',$4,$4)`, cliID, accountID, "client_"+suffix, now); err != nil {
		t.Fatal(err)
	}
	// A CLI session's key is trusted while the session can refresh.
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.cli_refresh_tokens(token_hash,cli_client_session_id,state,expires_at,created_at) VALUES($1,$2,'active',$3,$4)`, "refresh_"+cliID, cliID, now.Add(24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	cliPublic, cliPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cliRaw := signedFixture(t, cliPrivate, accountID, wire.RoleCLI, cliID, 1, 1, now.Add(-time.Minute), now.Add(time.Hour))
	cliCertificate, err := wire.Verify(cliRaw, cliPublic, wire.Expected{AccountID: accountID, Role: wire.RoleCLI, EndpointID: cliID, Generation: 1, Serial: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	cliCertificate.KeyID = keyIDForFingerprint(sha256.Sum256(cliPublic))
	cliCertificate.RootFingerprint = sha256.Sum256(cliPublic)
	if _, err := repository.BootstrapFresh(ctx, "operation_independent_cli_"+suffix, accountID, cliID, cliPublic, cliCertificate, now); err != nil {
		t.Fatalf("new CLI needs no account root: %v", err)
	}
	cliRenewAt := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	cliRenewRaw := signedFixture(t, cliPrivate, accountID, wire.RoleCLI, cliID, 1, 2, cliRenewAt.Add(-time.Minute), cliRenewAt.Add(2*time.Hour))
	cliRenew, err := wire.Verify(cliRenewRaw, cliPublic, wire.Expected{AccountID: accountID, Role: wire.RoleCLI, EndpointID: cliID, Generation: 1, Serial: 2}, cliRenewAt)
	if err != nil {
		t.Fatal(err)
	}
	cliRenew.KeyID = keyIDForFingerprint(sha256.Sum256(cliPublic))
	cliRenew.RootFingerprint = sha256.Sum256(cliPublic)
	if _, err := repository.BootstrapFresh(ctx, "operation_independent_cli_renew_"+suffix, accountID, cliID, cliPublic, cliRenew, cliRenewAt); err != nil {
		t.Fatalf("CLI certificate renewal needs no account root: %v", err)
	}
	if _, err := repository.BootstrapFresh(ctx, "operation_independent_cli_stale_"+suffix, accountID, cliID, cliPublic, cliCertificate, cliRenewAt); err == nil {
		t.Fatal("old CLI certificate was registered after renewal")
	}
	var rootCount, envKeyCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.account_e2ee_roots WHERE user_id=$1),(SELECT count(*) FROM paperboat.account_e2ee_keys WHERE user_id=$1)`, accountID).Scan(&rootCount, &envKeyCount); err != nil || rootCount != 0 || envKeyCount != 0 {
		t.Fatalf("transport enrollment changed ENV authority: roots=%d keys=%d err=%v", rootCount, envKeyCount, err)
	}
	request := MachineRegistrationRequest{OperationID: "operation_independent_" + suffix, UserID: accountID, MachineID: machineID, Generation: 3, Certificate: raw, Now: now}
	first, err := service.RegisterMachine(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.RegisterMachine(ctx, request)
	if err != nil || replay.Fingerprint != first.Fingerprint {
		t.Fatalf("idempotent replay=%+v err=%v", replay, err)
	}
	if _, err := service.Get(ctx, accountID, machineID, 3, now); err != nil {
		t.Fatalf("registered certificate is unavailable: %v", err)
	}
	keys, err := service.TransportKeys(ctx, accountID, now)
	if err != nil || len(keys.Keys) != 2 {
		t.Fatalf("transport keys=%+v err=%v", keys, err)
	}
	stale := request
	stale.Generation = 2
	if _, err := service.RegisterMachine(ctx, stale); !errors.Is(err, wire.ErrIdentity) {
		t.Fatalf("stale installation generation accepted: %v", err)
	}
	renewalNow := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)
	renewalRaw := signedFixture(t, private, accountID, wire.RoleMachine, machineID, 3, 2, renewalNow.Add(-time.Minute), renewalNow.Add(2*time.Hour))
	copy(renewalRaw[quicOffset:quicOffset+32], public)
	renewalPayload := renewalRaw[:len(renewalRaw)-ed25519.SignatureSize]
	copy(renewalRaw[len(renewalPayload):], ed25519.Sign(private, renewalPayload))
	renewal := request
	renewal.OperationID = "operation_renew_independent_" + suffix
	renewal.Certificate = renewalRaw
	renewal.Now = renewalNow
	if _, err := service.RegisterMachine(ctx, renewal); err != nil {
		t.Fatalf("machine certificate renewal: %v", err)
	}
	if _, err := service.Get(ctx, accountID, machineID, 3, renewalNow); err != nil {
		t.Fatalf("renewed certificate unavailable: %v", err)
	}
	if _, err := service.RegisterMachine(ctx, MachineRegistrationRequest{OperationID: "operation_stale_serial_" + suffix, UserID: accountID, MachineID: machineID, Generation: 3, Certificate: raw, Now: renewalNow}); err == nil {
		t.Fatal("old machine certificate was registered after renewal")
	}
	if _, err := service.Revoke(ctx, "operation_revoke_independent_"+suffix, accountID, machineID, 3, 2, "endpoint_removed", renewalNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, accountID, machineID, 3, renewalNow.Add(time.Second)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("revoked certificate still available: %v", err)
	}
	keys, err = service.TransportKeys(ctx, accountID, renewalNow.Add(time.Second))
	if err != nil || len(keys.Keys) != 1 || keys.Keys[0].Fingerprint != sha256.Sum256(cliPublic) {
		t.Fatalf("revoked machine key still trusted: %+v err=%v", keys, err)
	}
}
