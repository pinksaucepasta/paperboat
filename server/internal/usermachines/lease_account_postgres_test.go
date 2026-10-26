package usermachines

import (
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/machineaddress"
)

// A team machine's viewers each get their own local address on first sight (PLAN 3b "Multi-account hosts"): the
// owner's lease comes from pairing and the team admin's from the first list that shows it to them as an approved
// machine; both are slot 2 of their own account, and a second list adds nothing.
func TestTeamViewerGetsItsOwnLeaseOnFirstSightOnPostgres(t *testing.T) {
	f := newLeaseFixture(t)
	s := fmt.Sprint(time.Now().UnixNano())
	owner, admin, team, machine := "lease_owner_"+s, "lease_admin_"+s, "lease-team-"+s, "lease_machine_"+s
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, owner, admin)
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team',5,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+team, team)
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true)`, team, owner, admin)
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,owner_team_id)
VALUES($1,$2,$1,'shared-host','linux','amd64','/w','online','occupied',true,$3)`, machine, owner, team)
	// The machine is network-approved: it has a current peer identity and certificate.
	f.exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES(sha256(convert_to($1,'UTF8')),$2,$1,'machine',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('q'||$1,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('s'||$1,'UTF8')))`, machine, owner)
	f.exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
VALUES($1,$2,'machine',$2,1,1,1,sha256(convert_to('w'||$2,'UTF8')),sha256(convert_to($2,'UTF8')),'100.126.255.253',now(),now())`, owner, machine)
	// Another machine of the admin's own takes the admin's slot 2, so the viewer's lease is a different number
	// than the owner's.
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
VALUES($1,$2,$1,'admin-box','linux','amd64','/w','online','occupied',true)`, admin+"_own", admin)
	if got := f.slots(t, admin, admin+"_own"); got[admin+"_own"] != 2 {
		t.Fatalf("the admin's own machine: %v, want slot 2", got)
	}
	if got := f.slots(t, owner, machine); got[machine] != 2 {
		t.Fatalf("the owner's lease from pairing: %v, want slot 2", got)
	}

	service := New(f.store, nil, Policy{}, nil)
	list := func() UserMachine {
		t.Helper()
		items, _, _, err := service.ListInventory(f.ctx, admin, InventoryOptions{Limit: 10, Q: "shared-host"})
		if err != nil || len(items) != 1 {
			t.Fatalf("the admin's list: %+v, %v", items, err)
		}
		return items[0]
	}
	first := list()
	if want := machineaddress.FromSlot(3).String(); first.AssignedIP != want {
		t.Fatalf("the admin's address on first sight: %q, want its own next slot %s", first.AssignedIP, want)
	}
	if again := list(); again.AssignedIP != first.AssignedIP {
		t.Fatalf("the admin's address changed on the second list: %q -> %q", first.AssignedIP, again.AssignedIP)
	}
	var leases int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.control_sync_leases WHERE machine_id=$1`, machine).Scan(&leases); err != nil || leases != 2 {
		t.Fatalf("leases of the shared machine: %d, %v; want one per account that sees it (2)", leases, err)
	}
}
