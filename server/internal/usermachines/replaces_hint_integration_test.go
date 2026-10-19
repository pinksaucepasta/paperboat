package usermachines

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
)

// `pb reset` (start fresh) deletes the computer's own record and the new install arrives later,
// so the install names the record it replaces. The hint is honoured only for a retired personal
// record of the same account; the name and the local address slot carry over.
func TestFreshEnrollmentReplacesTheNamedRetiredRecord(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, otherID := "usr_replaces_"+suffix, "usr_replaces_other_"+suffix
	for _, id := range []string{userID, otherID} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, id, "workos_"+id, id+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',1,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_replaces_"+suffix, userID, "sub_replaces_"+suffix); err != nil {
		t.Fatal(err)
	}
	addMachine := func(id, owner, alias, state, seat string) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace',$5,$6)`, id, owner, "env_"+id, alias, state, seat); err != nil {
			t.Fatal(err)
		}
	}
	slotOf := func(id string) (slot int) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT address_slot FROM paperboat.ensure_control_sync_lease($1)`, id).Scan(&slot); err != nil {
			t.Fatal(err)
		}
		return
	}
	record := func(id string) (alias, state string, deleted bool) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT alias,state,deleted_at IS NOT NULL FROM paperboat.user_machines WHERE id=$1`, id).Scan(&alias, &state, &deleted); err != nil {
			t.Fatal(err)
		}
		return
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureBootstrapCommand("pb machine bootstrap")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_replaces_" + suffix, HelperID: "hlp_replaces_" + suffix, Credential: "enrollment-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	enrollments := 0
	enroll := func(alias, replaces string) string {
		t.Helper()
		enrollments++
		start, err := service.StartEnrollment(ctx, userID, fmt.Sprintf("idem-%d-%s", enrollments, suffix))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: start.BootstrapToken, Verifier: fmt.Sprintf("verifier-%d-%s", enrollments, suffix), Alias: alias, Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/workspace", RuntimeVersions: json.RawMessage(`{"pb":"test"}`), PublicIdentityKey: newIntegrationPublicIdentityKey(t), ReplacesMachineID: replaces}); err != nil {
			t.Fatal(err)
		}
		status, err := service.Enrollment(ctx, userID, start.ID)
		if err != nil {
			t.Fatal(err)
		}
		return status.UserMachineID
	}
	count := func(query string, args ...any) (n int) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}

	// 1. The old record is still there (disconnected): the install removes it, takes its exact
	// name and its local address slot, and the history keeps both audit rows.
	retired := "mch_replaces_retired_" + suffix
	addMachine(retired, userID, "studio", "disconnected", "released")
	retiredSlot := slotOf(retired)
	fresh := enroll("studio", retired)
	if alias, state, deleted := record(retired); state != "deleted" || !deleted || alias != "studio" {
		t.Fatalf("retired record: %q %q deleted=%v, want removed", alias, state, deleted)
	}
	if alias, _, _ := record(fresh); alias != "studio" {
		t.Fatalf("fresh machine alias=%q, want the exact name studio", alias)
	}
	if got := slotOf(fresh); got != retiredSlot {
		t.Fatalf("local address slot %d did not carry over (%d)", got, retiredSlot)
	}
	if n := count(`SELECT count(*) FROM paperboat.audit_events WHERE (resource_id=$1 AND event_type='user_machine.deleted') OR (resource_id=$2 AND event_type='user_machine.replaced')`, retired, fresh); n != 2 {
		t.Fatalf("audit rows kept = %d, want 2", n)
	}

	// 2. start fresh already removed the record before the install arrived: the exact name is free
	// and the slot carries over from the removed row.
	gone := "mch_replaces_gone_" + suffix
	addMachine(gone, userID, "desk", "online", "occupied")
	goneSlot := slotOf(gone)
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state='deleted',deleted_at=now(),seat_state='released' WHERE id=$1`, gone); err != nil {
		t.Fatal(err)
	}
	second := enroll("desk", gone)
	if alias, _, _ := record(second); alias != "desk" {
		t.Fatalf("after start fresh the alias=%q, want desk", alias)
	}
	if got := slotOf(second); got != goneSlot {
		t.Fatalf("local address slot %d did not carry over from the removed record (%d)", got, goneSlot)
	}

	// 3. The hint is a request, never authority: another account's record, a live machine of this
	// account and an unknown id change nothing, and the name stays numbered.
	foreign := "mch_replaces_foreign_" + suffix
	addMachine(foreign, otherID, "lab", "disconnected", "released")
	live := "mch_replaces_live_" + suffix
	addMachine(live, userID, "lab", "online", "occupied")
	liveSlot := slotOf(live)
	for _, hint := range []string{foreign, live, "mch_does_not_exist_" + suffix} {
		fresh := enroll("lab", hint)
		if alias, _, _ := record(fresh); alias != "lab-2" && alias != "lab-3" && alias != "lab-4" {
			t.Fatalf("hint %q: fresh alias=%q, want a numbered name", hint, alias)
		}
		if slotOf(fresh) == liveSlot {
			t.Fatalf("hint %q: the new machine took another record's local address", hint)
		}
	}
	if _, state, deleted := record(foreign); state != "disconnected" || deleted {
		t.Fatal("another account's retired record was touched")
	}
	if _, state, deleted := record(live); state != "online" || deleted {
		t.Fatal("a live machine was removed by a replace hint")
	}

	// 4. No hint: numbered name and a new slot, as before.
	plain := enroll("studio", "")
	if alias, _, _ := record(plain); alias == "studio" {
		t.Fatalf("no hint, yet the fresh machine took %q", alias)
	}
}
