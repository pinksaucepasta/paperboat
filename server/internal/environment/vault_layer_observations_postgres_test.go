package environment

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestPostgresLayerObservationsReceiptFencesAndRevocation(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set isolated PAPERBOAT_TEST_DATABASE_DSN for layer observation transactions")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	err = db.Migrate(setup, store)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	owner, foreign, machine := "lo_owner_"+suffix, "lo_foreign_"+suffix, "lo_machine_"+suffix
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM environment_vault_scopes WHERE owner_id=$1`, owner); err != nil {
			t.Error(err)
		}
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM users WHERE id IN($1,$2)`, owner, foreign); err != nil {
			t.Error(err)
		}
	}()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, foreign)
	exec(`INSERT INTO user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,configured_capabilities) VALUES($1,$2,$3,'layer observation fixture','linux','amd64','/layer-test','online','released',ARRAY['environment_injection'])`, machine, owner, "lo_env_"+suffix)
	public := bytes.Repeat([]byte{19}, 32)
	document := "sha256:" + strings.Repeat("a", 64)
	exec(`INSERT INTO environment_vault_hosts(machine_id,account_id,installation_generation,host_key_generation,host_public) VALUES($1,$2,1,1,$3)`, machine, owner, public)
	exec(`INSERT INTO environment_vault_scopes(workspace_id,owner_kind,owner_id,machine_id,key_epoch,revision,document_id,envelope,writer_public) VALUES('personal','personal',$1,'',1,1,$2,$3,$4)`, owner, document, []byte{1}, public)
	exec(`INSERT INTO environment_vault_layer_fences(workspace_id,owner_kind,owner_id,source_machine_id,generation) VALUES('personal','personal',$1,'',1)`, owner)
	exec(`INSERT INTO environment_vault_layers(machine_id,workspace_id,owner_kind,owner_id,source_machine_id,delivery_generation,document_id,installation_generation,host_key_generation,applied_fence_generation,writer_account,writer_public,envelope) VALUES($1,'personal','personal',$2,'',1,$3,1,1,1,$2,$4,$5)`, machine, owner, document, public, []byte{1})
	service := NewService(store, nil, nil, "https://control.example")
	observation := VaultLayerObservation{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: owner, DeliveryGeneration: 1, DocumentID: document, FenceGeneration: 1, HostRecipientKeyID: vaultHostKeyID(public), ObservationSeq: 2, State: "applied", ObservedAt: time.Now().UTC()}
	in := VaultLayerObservationsPut{OperationID: "lo_op_" + suffix, Observations: []VaultLayerObservation{observation}}
	if ack, err := service.RecordVaultLayerObservations(ctx, owner, machine, in); err != nil || !ack.Accepted || ack.OperationID != in.OperationID {
		t.Fatalf("current report: %+v %v", ack, err)
	}
	var delivery VaultLayerDelivery
	read := func() {
		t.Helper()
		err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var err error
			delivery, err = readVaultLayerTx(ctx, tx, machine, observation.coordinate())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	read()
	if !delivery.Applied || delivery.Observation == nil {
		t.Fatal("current report not exposed as applied")
	}
	changed := in
	changed.Observations = append([]VaultLayerObservation(nil), in.Observations...)
	changed.Observations[0].ObservationSeq++
	if _, err := service.RecordVaultLayerObservations(ctx, owner, machine, changed); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("operation mutation: %v", err)
	}
	changed.OperationID = "lo_same_" + suffix
	changed.Observations[0] = observation
	changed.Observations[0].ObservedAt = observation.ObservedAt.Add(time.Second)
	if _, err := service.RecordVaultLayerObservations(ctx, owner, machine, changed); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("same sequence mutation: %v", err)
	}
	changed.OperationID = "lo_old_" + suffix
	changed.Observations[0] = observation
	changed.Observations[0].ObservationSeq = 1
	if _, err := service.RecordVaultLayerObservations(ctx, owner, machine, changed); err != nil {
		t.Fatalf("stale sequence should be ignored: %v", err)
	}
	read()
	if delivery.Observation.ObservationSeq != 2 {
		t.Fatal("stale sequence replaced current report")
	}
	exec(`UPDATE environment_vault_layer_fences SET generation=2 WHERE workspace_id='personal' AND owner_kind='personal' AND owner_id=$1`, owner)
	read()
	if delivery.Applied {
		t.Fatal("advanced fence retained applied readiness")
	}
	if _, err := service.RecordVaultLayerObservations(ctx, owner, machine, in); err != nil {
		t.Fatalf("lost ACK receipt after fence advance: %v", err)
	}
	changed.OperationID = "lo_fence_" + suffix
	changed.Observations[0] = observation
	changed.Observations[0].ObservationSeq = 3
	if _, err := service.RecordVaultLayerObservations(ctx, owner, machine, changed); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale current binding: %v", err)
	}
	if _, err := service.RecordVaultLayerObservations(ctx, foreign, machine, in); !errors.Is(err, ErrMachineNotFound) {
		t.Fatalf("foreign recipient: %v", err)
	}
	exec(`UPDATE user_machines SET revoked_at=now() WHERE id=$1`, machine)
	if _, err := service.RecordVaultLayerObservations(ctx, owner, machine, in); !errors.Is(err, ErrMachineNotFound) {
		t.Fatalf("revoked replay: %v", err)
	}
}
