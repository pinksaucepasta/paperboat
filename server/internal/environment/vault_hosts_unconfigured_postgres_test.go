package environment

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestPostgresFreshHostBootstrapRequiresNeverConfiguredAccount(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run the host ENV bootstrap proof")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(dsn), "_test") {
		t.Fatal("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database")
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	migrationCtx, migrationCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer migrationCancel()
	if err := db.Migrate(migrationCtx, store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	freshAccount, markedAccount := "envfresh_"+suffix, "envmarked_"+suffix
	freshMachine, markedMachine := "envhost_"+suffix, "envmarkedhost_"+suffix
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id IN($1,$2)`, freshAccount, markedAccount); err != nil {
			t.Errorf("clean bootstrap fixtures: %v", err)
		}
	}()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, freshAccount, markedAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,configured_capabilities) VALUES ($1,$2,$3,'linux','amd64','/env-test','online','released',ARRAY['environment_injection']),($4,$5,$6,'linux','amd64','/env-test','online','released',ARRAY['environment_injection'])`, freshMachine, freshAccount, "env_"+suffix, markedMachine, markedAccount, "env_marked_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, "https://control.example")
	request := VaultHostKeyRequest{
		OperationID:            "fresh_host_" + suffix,
		InstallationGeneration: 1,
		HostKeyGeneration:      1,
		HostPublic:             b64Vault(bytes.Repeat([]byte{19}, 32)),
	}
	if binding, err := service.RegisterVaultHostKey(ctx, freshAccount, freshMachine, request); err != nil || binding.HostPublic != request.HostPublic {
		t.Fatalf("fresh encrypted recipient registration failed: %v", err)
	}
	if context, err := service.VaultLayerContext(ctx, freshAccount, freshMachine, freshAccount, "personal"); err != nil || len(context.Layers) != 0 || context.Recipient.MachineID != freshMachine {
		t.Fatalf("fresh authenticated empty context failed: %v", err)
	}
	var unconfiguredMarkerCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.environment_vault_account_configurations WHERE account_id=$1`, freshAccount).Scan(&unconfiguredMarkerCount); err != nil || unconfiguredMarkerCount != 0 {
		t.Fatalf("fresh account marker count=%d err=%v", unconfiguredMarkerCount, err)
	}

	vault := passwordVaultFixture(t, "https://control.example", freshAccount, 1, make([]byte, 32))
	if _, err := service.PutPasswordVault(ctx, freshAccount, vault); err != nil {
		t.Fatal(err)
	}
	var configuredGeneration uint64
	if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.environment_vault_account_configurations WHERE account_id=$1`, freshAccount).Scan(&configuredGeneration); err != nil || configuredGeneration == 0 {
		t.Fatalf("first vault publication marker generation=%d err=%v", configuredGeneration, err)
	}
	registered, err := service.RegisterVaultHostKey(ctx, freshAccount, freshMachine, request)
	if err != nil || registered.RecipientAccount != freshAccount {
		t.Fatalf("configured host registration recipient=%q err=%v", registered.RecipientAccount, err)
	}
	retry, err := service.RegisterVaultHostKey(ctx, freshAccount, freshMachine, request)
	if err != nil || retry.RecipientAccount != freshAccount || retry.HostPublic != registered.HostPublic {
		t.Fatalf("configured pending retry recipient=%q err=%v", retry.RecipientAccount, err)
	}

	// A configured scope anchor is not an assignment. Fresh recipients need
	// no encrypted key publication until its current record set has a value.
	coordinate := VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: freshAccount}
	anchor := vaultScopeFixture(t, fixtureScopeHeader(freshAccount, "personal", freshAccount, 1, 1, 1, make([]byte, 32)), 11)
	if _, err := service.PutVaultScope(ctx, freshAccount, "personal", "personal", freshAccount, "", VaultScopePut{OperationID: "empty_anchor_" + suffix, Envelope: b64Vault(anchor)}); err != nil {
		t.Fatal(err)
	}
	assertContext := func(wantLayers int) VaultLayerContext {
		t.Helper()
		bundle, err := service.VaultLayerContext(ctx, freshAccount, freshMachine, freshAccount, "personal")
		if err != nil || len(bundle.Layers) != wantLayers || bundle.Recipient.MachineID != freshMachine || bundle.Recipient.HostPublic != registered.HostPublic || bundle.Recipient.InstallationGeneration != registered.InstallationGeneration {
			t.Fatalf("configured context layers=%d want=%d recipient=%+v err=%v", len(bundle.Layers), wantLayers, bundle.Recipient, err)
		}
		return bundle
	}
	assertContext(0)
	recordID := strings.Repeat("1", 64)
	mutate := func(revision, expected uint64, deleted bool) VaultRecordsPage {
		t.Helper()
		page, err := service.PutVaultRecords(ctx, freshAccount, coordinate, VaultRecordsPut{OperationID: "empty_record_" + suffix + "_" + strings.Repeat("r", int(revision)), Records: []VaultRecordMutation{{ExpectedRevision: expected, Envelope: vaultRecordFixture(t, coordinate, freshAccount, recordID, revision, 1, deleted)}}})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	mutate(1, 0, false)
	if bundle := assertContext(1); bundle.Layers[0].State != "pending" || bundle.Layers[0].Envelope != "" {
		t.Fatal("nonempty unpublished scope lost its pending delivery guard")
	}
	if page := mutate(2, 1, true); page.Sequence == 0 || len(page.Records) != 1 || !page.Records[0].Deleted {
		t.Fatal("signed deletion did not retain its nonzero record history")
	}
	assertContext(0)
	if _, err := service.VaultLayerContext(ctx, freshAccount, freshMachine, markedAccount, "personal"); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatalf("empty scope bypassed actor authorization: %v", err)
	}

	// A marker without a current vault represents prior configuration and must
	// remain fail closed rather than creating the empty-state bootstrap signal.
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.environment_vault_account_configurations(account_id,generation) VALUES($1,1)`, markedAccount); err != nil {
		t.Fatal(err)
	}
	markedRequest := VaultHostKeyRequest{
		OperationID:            "marked_host_" + suffix,
		InstallationGeneration: 1,
		HostKeyGeneration:      1,
		HostPublic:             b64Vault(bytes.Repeat([]byte{23}, 32)),
	}
	if _, err := service.RegisterVaultHostKey(ctx, markedAccount, markedMachine, markedRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VaultLayerContext(ctx, markedAccount, markedMachine, markedAccount, "personal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("prior configured account without vault silently emptied: %v", err)
	}
}
