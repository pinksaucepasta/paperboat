package selfhost

import (
	"testing"
	"time"
)

// A drained server says so: draining and drain_deadline come from its node's state, in the list, the inventory, the
// single read and the drain and resume answers; ready is false meanwhile.
func TestInstallationsReportDraining(t *testing.T) {
	f := newDrainFixture(t)
	ctx := t.Context()
	owner := f.user("owner")
	scope := Scope{Kind: "account", ID: owner}
	draining := scopedInstallationFixture(t, f.store, "drain-test-key-with-at-least-32-bytes", owner, "tunnel")
	steady := scopedInstallationFixture(t, f.store, "drain-test-key-with-at-least-32-bytes", owner, "tunnel")
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true,last_heartbeat_at=now(),public_ingress_verified_at=now() WHERE id=ANY($1)`, []string{draining.NodeID, steady.NodeID})
	read := func() (list, inventory, single map[string]Installation) {
		t.Helper()
		l, err := f.service.ListScoped(ctx, owner, scope, false)
		if err != nil {
			t.Fatal(err)
		}
		inv, err := f.service.ListScopedInventory(ctx, owner, scope, false, InventoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		list, inventory, single = map[string]Installation{}, map[string]Installation{}, map[string]Installation{}
		for _, i := range l {
			list[i.InstallationID] = i
		}
		for _, i := range inv.Installations {
			inventory[i.InstallationID] = i
		}
		for _, id := range []string{draining.InstallationID, steady.InstallationID} {
			one, err := f.service.GetInstallation(ctx, owner, id, false)
			if err != nil {
				t.Fatal(err)
			}
			single[id] = one
		}
		return
	}
	check := func(label string, wantDraining bool) {
		t.Helper()
		list, inventory, single := read()
		for source, items := range map[string]map[string]Installation{"list": list, "inventory": inventory, "single": single} {
			d, s := items[draining.InstallationID], items[steady.InstallationID]
			if d.Draining != wantDraining || (d.DrainDeadline != nil) != wantDraining || d.Ready && wantDraining {
				t.Fatalf("%s %s: draining server: %+v", label, source, d)
			}
			if s.Draining || s.DrainDeadline != nil || !s.Ready {
				t.Fatalf("%s %s: other server must be untouched and ready: %+v", label, source, s)
			}
			if wantDraining && time.Until(*d.DrainDeadline) < 9*time.Minute {
				t.Fatalf("%s %s: plain drain deadline %v", label, source, d.DrainDeadline)
			}
		}
	}
	check("before", false)
	item, err := f.service.DrainScoped(ctx, owner, draining.InstallationID, false, false)
	if err != nil || !item.Draining || item.DrainDeadline == nil || item.Ready {
		t.Fatalf("drain answer: %+v %v", item, err)
	}
	check("after drain", true)
	// Drain and move shortens the deadline to about 90 seconds.
	if item, err = f.service.DrainScoped(ctx, owner, draining.InstallationID, false, true); err != nil || !item.Draining || item.DrainDeadline == nil || time.Until(*item.DrainDeadline) > 2*time.Minute {
		t.Fatalf("drain and move answer: %+v %v", item, err)
	}
	item, err = f.service.ResumeScoped(ctx, owner, draining.InstallationID, false)
	if err != nil || item.Draining || item.DrainDeadline != nil {
		t.Fatalf("resume answer: %+v %v", item, err)
	}
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true,last_heartbeat_at=now() WHERE id=$1`, draining.NodeID)
	check("after resume", false)
}
