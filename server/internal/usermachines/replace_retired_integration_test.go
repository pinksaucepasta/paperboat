package usermachines

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/managedssh"
	"golang.org/x/crypto/ssh"
)

func hostKeyLine(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// A computer that is added again reports the SSH host keys of its dead record; that proves it is
// the same computer, so the dead record is removed through the normal removal and the new
// record takes the exact name. Different computers and other accounts are never touched.
func TestAddingTheSameComputerAgainRemovesItsRetiredRecordAndKeepsTheName(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userA, userB := "usr_replace_a_"+suffix, "usr_replace_b_"+suffix
	for _, id := range []string{userA, userB} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, id, "workos_"+id, id+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	addMachine := func(id, userID, alias, state string) {
		t.Helper()
		seat, online := "occupied", state == "online"
		if state == "disconnected" {
			seat = "released"
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace',$5,$6,$7,1)`, id, userID, "env_"+id, alias, state, seat, online); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	repository, err := managedssh.NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	sshService, _ := managedssh.NewService(repository.WithMachineReplacer(service))
	now := time.Now().UTC().Truncate(time.Second)
	observe := func(userID, machineID string, keys ...string) error {
		t.Helper()
		_, err := sshService.ObserveHost(ctx, managedssh.ObserveHostRequest{OperationID: fmt.Sprintf("operation_replace_%s_%d", machineID, time.Now().UnixNano()), UserID: userID, UserMachineID: machineID, MachineGeneration: 1, ObservationGeneration: 1, PublicKeys: keys, Now: now})
		return err
	}
	slotOf := func(id string) (slot int) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT leased_slot FROM paperboat.ensure_machine_address_leases((SELECT user_id FROM paperboat.user_machines WHERE id=$1), ARRAY[$1]::text[])`, id).Scan(&slot); err != nil {
			t.Fatal(err)
		}
		return
	}
	state := func(id string) (alias, status string, deleted bool) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT alias,state,deleted_at IS NOT NULL FROM paperboat.user_machines WHERE id=$1`, id).Scan(&alias, &status, &deleted); err != nil {
			t.Fatal(err)
		}
		return
	}

	// 1. Same computer: the retired record owned the host keys the new machine now reports.
	keys := []string{hostKeyLine(t), hostKeyLine(t)}
	oldID, newID := "mch_old_"+suffix, "mch_new_"+suffix
	addMachine(oldID, userA, "studio", "online")
	if err := observe(userA, oldID, keys...); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state='disconnected', seat_state='released', online=false WHERE id=$1`, oldID); err != nil {
		t.Fatal(err)
	}
	addMachine(newID, userA, "studio-2", "online")
	oldSlot, newOwnSlot := slotOf(oldID), slotOf(newID)
	if oldSlot == newOwnSlot {
		t.Fatalf("both records share local address slot %d before the re-add", oldSlot)
	}
	if err := observe(userA, newID, keys...); err != nil {
		t.Fatalf("the same computer added again could not record its host keys: %v", err)
	}
	if alias, status, deleted := state(oldID); status != "deleted" || !deleted || alias != "studio" {
		t.Fatalf("retired record: alias=%q state=%q deleted=%v, want removed", alias, status, deleted)
	}
	if alias, status, _ := state(newID); alias != "studio" || status != "online" {
		t.Fatalf("new record: alias=%q state=%q, want studio online", alias, status)
	}
	if got := slotOf(newID); got != oldSlot {
		t.Fatalf("the re-added computer's local address slot changed from %d to %d", oldSlot, got)
	}
	var claims, history int
	_ = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.machine_ssh_host_key_owners WHERE user_machine_id=$1`, oldID).Scan(&claims)
	_ = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.audit_events WHERE (resource_id=$1 AND event_type='user_machine.deleted') OR (resource_id=$2 AND event_type='user_machine.replaced')`, oldID, newID).Scan(&history)
	if claims != 0 || history != 2 {
		t.Fatalf("host-key claims left on the removed record=%d, audit rows kept=%d (want 0 and 2)", claims, history)
	}

	// 2. A different computer with the same name keeps its numbered name and the retired record.
	otherRetired, otherNew := "mch_otherold_"+suffix, "mch_othernew_"+suffix
	addMachine(otherRetired, userA, "lab", "disconnected")
	addMachine(otherNew, userA, "lab-2", "online")
	retiredSlot, otherSlot := slotOf(otherRetired), slotOf(otherNew)
	if err := observe(userA, otherNew, hostKeyLine(t)); err != nil {
		t.Fatal(err)
	}
	if slotOf(otherNew) != otherSlot || slotOf(otherRetired) != retiredSlot || otherSlot == retiredSlot {
		t.Fatal("a different computer's local address was changed or shared")
	}
	if alias, status, deleted := state(otherRetired); status != "disconnected" || deleted || alias != "lab" {
		t.Fatalf("an unrelated retired record was touched: %q %q %v", alias, status, deleted)
	}
	if alias, _, _ := state(otherNew); alias != "lab-2" {
		t.Fatalf("a different computer took the name: %q", alias)
	}

	// 3. Another account's retired record is never touched, even when the host keys match, and is no conflict.
	foreignKey := hostKeyLine(t)
	foreignOld, foreignNew := "mch_foreignold_"+suffix, "mch_foreignnew_"+suffix
	addMachine(foreignOld, userB, "desk", "online")
	if err := observe(userB, foreignOld, foreignKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state='disconnected', seat_state='released', online=false WHERE id=$1`, foreignOld); err != nil {
		t.Fatal(err)
	}
	addMachine(foreignNew, userA, "desk", "online")
	var inUse *managedssh.HostKeyInUseError
	if err := observe(userA, foreignNew, foreignKey); err != nil {
		t.Fatalf("keys another account's machine holds are not a conflict: %v", err)
	}
	if _, status, deleted := state(foreignOld); status != "disconnected" || deleted {
		t.Fatalf("another account's record was touched: %q %v", status, deleted)
	}

	// 4. A live machine is never removed.
	liveKey := hostKeyLine(t)
	liveOld, liveNew := "mch_liveold_"+suffix, "mch_livenew_"+suffix
	addMachine(liveOld, userA, "rack", "online")
	if err := observe(userA, liveOld, liveKey); err != nil {
		t.Fatal(err)
	}
	addMachine(liveNew, userA, "rack-2", "online")
	if err := observe(userA, liveNew, liveKey); !errors.As(err, &inUse) || inUse.MachineName != "rack" {
		t.Fatalf("a live machine's host key was handed over, or the machine was not named: %v", err)
	}
	if _, status, deleted := state(liveOld); status != "online" || deleted {
		t.Fatalf("a live machine was removed: %q %v", status, deleted)
	}
}

// F1-09/F1-10: pb reset no longer removes the old record; pb pair replaces it only after the new machine
// works. The replacement takes the old local address and exact name in one transaction, even while the old
// record still looks live, a retry is a no-op, and another account's record is never touched.
func TestReplaceMachineCutsOverToTheVerifiedNewRecordAndKeepsTheAddress(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userA, userB := "usr_cutover_a_"+suffix, "usr_cutover_b_"+suffix
	for _, id := range []string{userA, userB} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, id, "workos_"+id, id+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	addMachine := func(id, userID, alias string) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',true,1)`, id, userID, "env_"+id, alias); err != nil {
			t.Fatal(err)
		}
	}
	slotOf := func(id string) (slot int) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT leased_slot FROM paperboat.ensure_machine_address_leases((SELECT user_id FROM paperboat.user_machines WHERE id=$1), ARRAY[$1]::text[])`, id).Scan(&slot); err != nil {
			t.Fatal(err)
		}
		return
	}
	state := func(id string) (alias string, deleted bool) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT alias,deleted_at IS NOT NULL FROM paperboat.user_machines WHERE id=$1`, id).Scan(&alias, &deleted); err != nil {
			t.Fatal(err)
		}
		return
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})

	// The account already has machines below the old one, so a fresh allocation would move the address.
	filler, oldID, newID := "mch_cfill_"+suffix, "mch_cold_"+suffix, "mch_cnew_"+suffix
	addMachine(filler, userA, "other")
	addMachine(oldID, userA, "studio")
	slotOf(filler)
	oldSlot := slotOf(oldID)
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE id=$1`, filler); err != nil {
		t.Fatal(err)
	}
	addMachine(newID, userA, "studio-2")
	if own := slotOf(newID); own == oldSlot {
		t.Fatalf("fixture: the new record already has the old slot %d", own)
	}
	if err := service.ReplaceMachine(ctx, userA, oldID, newID); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, deleted := state(oldID); !deleted {
		t.Fatal("the old record was not removed after the cutover")
	}
	if alias, deleted := state(newID); alias != "studio" || deleted {
		t.Fatalf("new record: alias=%q deleted=%v, want studio kept", alias, deleted)
	}
	if got := slotOf(newID); got != oldSlot {
		t.Fatalf("the local address slot changed across reset: %d -> %d", oldSlot, got)
	}
	if err := service.ReplaceMachine(ctx, userA, oldID, newID); err != nil {
		t.Fatalf("a retry after the cutover must be a no-op: %v", err)
	}

	// A computer that was renamed keeps its name across a reset, although the new record came up under the
	// hostname (not a numbered variant of the old name).
	renamedOld, renamedNew := "mch_crold_"+suffix, "mch_crnew_"+suffix
	addMachine(renamedOld, userA, "apple")
	addMachine(renamedNew, userA, "manageds-virtual-machine")
	if err := service.ReplaceMachine(ctx, userA, renamedOld, renamedNew); err != nil {
		t.Fatalf("replace a renamed computer: %v", err)
	}
	if alias, deleted := state(renamedNew); alias != "apple" || deleted {
		t.Fatalf("a renamed computer lost its name across the cutover: alias=%q deleted=%v", alias, deleted)
	}

	// Another account's record is never touched.
	foreign := "mch_cforeign_" + suffix
	addMachine(foreign, userB, "desk")
	if err := service.ReplaceMachine(ctx, userA, foreign, newID); err != nil {
		t.Fatalf("another account's record: %v", err)
	}
	if _, deleted := state(foreign); deleted {
		t.Fatal("another account's record was removed")
	}
	third := "mch_cthird_" + suffix
	addMachine(third, userA, "rack")
	if err := service.ReplaceMachine(ctx, userA, third, "mch_cmissing_"+suffix); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing replacement: %v, want ErrNotFound", err)
	}
	if _, deleted := state(third); deleted {
		t.Fatal("the old record was removed although the replacement does not exist")
	}
}
