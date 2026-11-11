package usermachines

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// P0 (2026-10-26): since the machine list stopped allocating local address leases (05862b9b), a machine that
// never connected control sync had none, so it had no address and private access to it failed (14 machines on
// production). Pairing gives the machine its lease in the pairing transaction: it is listed with its address.
func TestPairedMachineHasALocalAddressLease(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_pair_lease_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "pair-lease-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',100,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_pair_lease_"+suffix, userID, "sub_pair_lease_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("pair-lease-key")
	service.ConfigureAccess(nil, "https://api.example.test")
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_pair_lease_" + suffix, HelperID: "helper_pair_lease_" + suffix, Credential: "pair-lease-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool)
	slots := make(map[int]bool)
	for i := range 100 {
		start, err := service.StartEnrollment(ctx, userID, fmt.Sprintf("idem-pair-lease-%s-%d", suffix, i))
		if err != nil {
			t.Fatal(err)
		}
		pairing, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: start.BootstrapToken, Verifier: fmt.Sprintf("pair-lease-verifier-%s-%d", suffix, i), Alias: fmt.Sprintf("fresh-%03d", i), Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: newIntegrationPublicIdentityKey(t), RuntimeVersions: json.RawMessage(`{"pb":"test"}`)})
		if err != nil {
			t.Fatal(err)
		}
		machine, err := service.Approve(ctx, userID, pairing.UserCode)
		if err != nil {
			t.Fatal(err)
		}
		var slot int
		if err := store.SQL().QueryRowContext(ctx, `SELECT address_slot FROM paperboat.control_sync_leases WHERE machine_id=$1`, machine.ID).Scan(&slot); err != nil {
			t.Fatalf("the paired machine %s has no local address lease: %v", machine.ID, err)
		}
		if ids[machine.ID] || slots[slot] {
			t.Fatal("pairing reused a machine or address")
		}
		ids[machine.ID], slots[slot] = true, true
	}
	started := time.Now()

	items, total, more, err := service.ListInventory(workspace.WithContext(ctx, workspace.Scope{ID: "personal"}), userID, InventoryOptions{Limit: 100})
	elapsed := time.Since(started)
	if err != nil || len(items) != 100 || total != 100 || more || elapsed >= time.Second {
		t.Fatalf("first list after 100 real pairings: %d of %d, more=%v, time=%v, err=%v", len(items), total, more, elapsed, err)
	}
	for _, item := range items {
		if !ids[item.ID] || item.AssignedIP == "" {
			t.Fatalf("listed unexpected/unleased machine %s", item.ID)
		}
		delete(ids, item.ID)
	}
	if len(ids) != 0 {
		t.Fatal("list omitted paired machines")
	}
	t.Logf("first list after 100 real pairings: %v", elapsed)
}
