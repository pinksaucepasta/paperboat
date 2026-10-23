package managedssh

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// The same computer enrolled again (pb reset, reinstall) gets a new machine record but keeps its
// SSH host keys. The retired record must not hold them against the new one forever.
func TestSQLRepositoryHostKeysMoveToTheMachineThatReplacedARetiredRecord(t *testing.T) {
	hostKeysMoveToTheReplacement(t, "disconnected")
}

// An OFFLINE record of the same account that cannot answer is released the same way: the computer was added again.
func TestSQLRepositoryHostKeysMoveFromAnOfflineRecordOfTheSameAccount(t *testing.T) {
	hostKeysMoveToTheReplacement(t, "offline")
}

func hostKeysMoveToTheReplacement(t *testing.T, retiredState string) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run managed SSH repository integration tests")
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
	user, other := "reenroll_user_"+suffix, "reenroll_other_"+suffix
	oldID, newID, otherID, liveID := "reenroll_old_"+suffix, "reenroll_new_"+suffix, "reenroll_foreign_"+suffix, "reenroll_live_"+suffix
	for _, userID := range []string{user, other} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+userID, userID+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []struct{ id, userID string }{{oldID, user}, {newID, user}, {liveID, user}, {otherID, other}} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, value.id, value.userID, "env_"+value.id); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(repository)
	observe := func(operation, userID, machineID string, keys ...string) error {
		_, err := service.ObserveHost(ctx, ObserveHostRequest{OperationID: operation + suffix, UserID: userID, UserMachineID: machineID, MachineGeneration: 1, ObservationGeneration: 1, PublicKeys: keys, Now: now})
		return err
	}
	hostKey := publicLine(t, "ed25519")
	if err := observe("operation_reenroll_old", user, oldID, hostKey); err != nil {
		t.Fatal(err)
	}
	// A live machine's host keys are refused to another machine with a typed error naming the
	// owner the account can see (an image cloned with its /etc/ssh keys).
	var inUse *HostKeyInUseError
	if err := observe("operation_reenroll_live", user, liveID, hostKey); !errors.As(err, &inUse) || inUse.MachineName == "" {
		t.Fatalf("a live machine's host key was handed to another machine, or the owner was not named: %v", err)
	}
	conflictOf := func(machineID string) string {
		var other string
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT other_machine_id FROM paperboat.machine_ssh_host_key_conflicts WHERE user_machine_id=$1),'')`, machineID).Scan(&other); err != nil {
			t.Fatal(err)
		}
		return other
	}
	if conflictOf(liveID) != oldID {
		t.Fatal("the refused observation was not recorded for the machine list")
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state=$2,online=false WHERE id=$1`, oldID, retiredState); err != nil {
		t.Fatal(err)
	}
	if err := observe("operation_reenroll_foreign", other, otherID, hostKey); !errors.As(err, &inUse) || inUse.MachineName != "" {
		t.Fatalf("another account took a retired record's host key, or its owner was named to it: %v", err)
	}
	if err := observe("operation_reenroll_new", user, newID, hostKey); err != nil {
		t.Fatalf("the replacement machine could not record the host keys it now has: %v", err)
	}
	if err := observe("operation_reenroll_live_fresh", user, liveID, publicLine(t, "ed25519")); err != nil {
		t.Fatalf("a machine with its own keys could not record them: %v", err)
	}
	if conflictOf(liveID) != "" {
		t.Fatal("a successful observation left the host-key conflict in the machine list")
	}
	active, err := service.GetActiveHost(ctx, GetHostKeySetRequest{ActorUserID: user, UserMachineID: newID, MachineGeneration: 1})
	if err != nil || len(active.Keys) != 1 {
		t.Fatalf("active host keys of the replacement=%+v error=%v", active, err)
	}
}
