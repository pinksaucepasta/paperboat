package usermachines

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/activity"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestFinalDeleteWaitsForRevocationAndRetainsAccounting(t *testing.T) {
	for _, action := range []string{"delete", "revoke", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			suffix := fmt.Sprint(time.Now().UnixNano())
			owner, machine, env := "final_owner_"+suffix, "final_machine_"+suffix, "final_env_"+suffix
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner)
			exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, owner)
			exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'original-name','linux','amd64','/workspace','offline','released')`, machine, owner, env)
			exec(`INSERT INTO user_machine_entitlements(id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES($1,$1,$1,'test','active',1,1000,date_trunc('month',now()),date_trunc('month',now())+interval '1 month')`, owner)
			exec(`INSERT INTO user_machine_bandwidth_periods(user_machine_id,period_start,period_end,included_bytes,consumed_included_bytes,consumed_topup_bytes) SELECT $1,current_period_start,current_period_end,1000,123,45 FROM user_machine_entitlements WHERE user_id=$2`, machine, owner)
			before, err := store.Queries().GetUserMachineBandwidthUsage(ctx, owner)
			if err != nil {
				t.Fatal(err)
			}
			session := newID("session")
			token := newID("token")
			exec(`INSERT INTO user_machine_access_sessions(id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,credential_kind,expires_at) VALUES($1,$2,$3,$4,'fixture-client','https://host.example.test',$5,'external_issuer',now()-interval '1 year')`, session, machine, owner, env, token)
			exec(`INSERT INTO control_helpers(id,environment_id) VALUES($1,$2)`, "helper_"+suffix, env)
			exec(`INSERT INTO user_favorites(user_id,kind,resource_id) VALUES($1,'machine',$2)`, owner, machine)
			issuer := &recordingIssuer{failRevocation: true}
			service := New(store, audit.NewWriter(store), Policy{}, nil)
			service.ConfigureAccess(issuer, "https://api.example.test")
			if _, err := service.Rename(ctx, owner, machine, "renamed-host"); err != nil {
				t.Fatal(err)
			}
			removalEvent := "user_machine.deleted"
			if action == "revoke" {
				exec(`UPDATE user_machines SET state='revoked' WHERE id=$1`, machine)
				removalEvent = "user_machine.revoked"
			} else if action == "disconnect" {
				if err := service.Disconnect(ctx, owner, machine); err == nil {
					t.Fatal("offline revocation unexpectedly succeeded")
				}
				removalEvent = "user_machine.disconnected"
			} else if err := service.Delete(ctx, owner, machine); err == nil {
				t.Fatal("offline revocation unexpectedly succeeded")
			}
			var envRevoked bool
			if err := store.SQL().QueryRowContext(ctx, `SELECT desired_state='revoked' FROM control_environments WHERE id=$1`, env).Scan(&envRevoked); err != nil || !envRevoked {
				t.Fatalf("%s did not revoke environment: %v %v", action, envRevoked, err)
			}

			if err := service.RetryPendingRevocations(ctx); err == nil {
				t.Fatal("offline retry unexpectedly succeeded")
			}
			var retained bool
			if err := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_machines WHERE id=$1 AND deleted_at IS NOT NULL) AND EXISTS(SELECT 1 FROM user_machine_access_sessions WHERE id=$2 AND helper_revoked_at IS NULL)`, machine, session).Scan(&retained); err != nil || !retained {
				t.Fatalf("undelivered revocation lost its tombstone: %v %v", retained, err)
			}
			issuer.failRevocation = false
			if err := service.RetryPendingRevocations(ctx); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"user_machines", "control_environments", "control_helpers", "user_machine_access_sessions", "user_favorites"} {
				var count int
				query := map[string]string{
					"user_machines":                `SELECT count(*) FROM user_machines WHERE id=$1`,
					"control_environments":         `SELECT count(*) FROM control_environments WHERE id=$1`,
					"control_helpers":              `SELECT count(*) FROM control_helpers WHERE environment_id=$1`,
					"user_machine_access_sessions": `SELECT count(*) FROM user_machine_access_sessions WHERE user_machine_id=$1`,
					"user_favorites":               `SELECT count(*) FROM user_favorites WHERE resource_id=$1`,
				}[table]
				arg := machine
				if table == "control_environments" || table == "control_helpers" {
					arg = env
				}
				if err := store.SQL().QueryRowContext(ctx, query, arg).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s remaining=%d: %v", table, count, err)
				}
			}
			after, err := store.Queries().GetUserMachineBandwidthUsage(ctx, owner)
			if err != nil || after != before || after.ConsumedIncludedBytes != 123 || after.ConsumedTopupBytes != 45 {
				t.Fatalf("account totals changed: before=%+v after=%+v error=%v", before, after, err)
			}
			var id, name string
			if err := store.SQL().QueryRowContext(ctx, `SELECT user_machine_id,machine_name FROM user_machine_bandwidth_periods WHERE user_id=$1`, owner).Scan(&id, &name); err != nil || id != machine || name != "original-name" {
				t.Fatalf("accounting identity=%s/%s: %v", id, name, err)
			}
			for _, event := range []string{"user_machine.renamed", removalEvent, "user_machine.final_deleted"} {
				var count int
				if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE resource_id=$1 AND event_type=$2`, machine, event).Scan(&count); err != nil || count != 1 {
					t.Fatalf("%s audit count=%d: %v", event, count, err)
				}
			}
			page, err := activity.New(store, config.Business{AuditRetentionFree: 24 * time.Hour}).List(ctx, owner, workspace.Scope{ID: "personal"}, activity.Query{MachineID: machine, Action: "user_machine.final_deleted"})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("final deletion missing from machine activity: %+v: %v", page, err)
			}
			if event := page.Items[0]; event.Machine == nil || event.Machine.ID != machine || event.Machine.Name != "renamed-host" || event.Target.Name != "renamed-host" {
				t.Fatalf("final deletion lost machine identity: %+v", event)
			}
			if err := service.RetryPendingRevocations(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFinalDeleteBatchIsBounded(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "final_batch_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,deleted_at) SELECT $1||n,$1,$1||n,'batch-'||n,'linux','amd64','/workspace','deleted','released','2000-01-01'::timestamptz FROM generate_series(1,101) n`, owner); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureAccess(&recordingIssuer{}, "https://api.example.test")
	if err := service.RetryPendingRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM user_machines WHERE user_id=$1`, owner).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("bounded tick remaining=%d: %v", remaining, err)
	}
	if err := service.RetryPendingRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM user_machines WHERE user_id=$1`, owner).Scan(&count); err != nil || count != 0 {
		t.Fatalf("batch remainder=%d: %v", count, err)
	}
}

type removedMachineFixture struct {
	store          *db.DB
	owner, machine string
	exec           func(query string, args ...any)
}

// newRemovedMachine inserts a removed machine with every row kind whose FK once blocked its final deletion:
// a redeemed device grant, its token redemption, a lazy policy with an activation on a preview, and a tunnel
// with a three-generation config chain.
func newRemovedMachine(t *testing.T, store *db.DB, owner, name string) removedMachineFixture {
	t.Helper()
	ctx := context.Background()
	f := removedMachineFixture{store: store, owner: owner, machine: name + "_machine"}
	f.exec = func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	env, session, grant, policy, preview, tunnel := name+"_env", name+"_cli", name+"_grant", name+"_policy", name+"_preview", name+"_tunnel"
	f.exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, owner)
	f.exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,deleted_at,revoked_at) VALUES($1,$2,$3,'removed-host','linux','amd64','/workspace','revoked','released',now(),now())`, f.machine, owner, env)
	f.exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,user_machine_id) VALUES($1,$2,$1,'final','desktop','linux',ARRAY['machines:connect'],'revoked',now(),now(),$3)`, session, owner, f.machine)
	f.exec(`INSERT INTO device_grants(id,client_id,client_label,device_type,os,scopes,device_code_hash,user_code_hash,state,issued_at,expires_at,poll_interval_seconds,next_poll_at,created_network_hash,user_id,cli_client_session_id)
 VALUES($1,$1,'final','desktop','linux',ARRAY['machines:connect'],'dc_'||$1,'uc_'||$1,'consumed',now(),now()+interval '1 hour',5,now(),'net',$2,$3)`, grant, owner, session)
	f.exec(`INSERT INTO device_token_redemptions(grant_id,cli_client_session_id,response_ciphertext,access_expires_at,expires_at) VALUES($1,$2,'\x00',now()+interval '1 hour',now()+interval '1 hour')`, grant, session)
	f.exec(`INSERT INTO preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,access_mode,lease_deadline) VALUES($1,'https://'||replace($1,'_','-')||'.example.test',$2,$2,$3,$1,'foreground','http','127.0.0.1:3000','public',now()+interval '1 hour')`, preview, owner, f.machine)
	f.exec(`INSERT INTO lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at) VALUES($1,replace($1,'_','-')||'.example.test',$2,$3,$4,1,1,'http','127.0.0.1:22','private','persistent_port',now()+interval '1 hour')`, policy, owner, env, f.machine)
	f.exec(`INSERT INTO lazy_activations(policy_id,policy_generation,boot_id,activation_id,owner_session_id,deadline,preview_id,state,cooldown_until) VALUES($1,1,'boot',$1||'_activation','session_01234567-89ab-4cde-8fab-0123456789ab',now()+interval '1 hour',$2,'running',now())`, policy, preview)
	f.exec(`INSERT INTO tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) SELECT $1,$2,$1,'active','public',1,'endpoint_'||u,'https://'||u||'.tunnels.example.test',$3,$2 FROM (SELECT gen_random_uuid()::text u) x`, tunnel, owner, f.machine)
	f.exec(`INSERT INTO tunnel_config_generations(tunnel_id,generation,previous_generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until)
 SELECT $1,g,CASE WHEN g>1 THEN g-1 END,sha256(convert_to($1||g,'UTF8')),'\x7b7d','pending',$2,now()+interval '1 day' FROM generate_series(1,3) g`, tunnel, owner)
	return f
}

func (f removedMachineFixture) exists(t *testing.T) bool {
	t.Helper()
	var exists bool
	if err := f.store.SQL().QueryRowContext(context.Background(), `SELECT EXISTS(SELECT 1 FROM user_machines WHERE id=$1)`, f.machine).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func finalDeleteTestService(store *db.DB) *Service {
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureAccess(&recordingIssuer{}, "https://api.example.test")
	return service
}

func newFinalDeleteOwner(t *testing.T, store *db.DB, suffix string) string {
	t.Helper()
	owner := "final_fk_" + suffix
	if _, err := store.SQL().ExecContext(context.Background(), `INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner); err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestFinalDeleteRemovesRowsThatBlockedTheCascade(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := newFinalDeleteOwner(t, store, suffix)
	f := newRemovedMachine(t, store, owner, "fk"+suffix)
	if err := finalDeleteTestService(store).RetryPendingRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	if f.exists(t) {
		t.Fatal("removed machine with grants, activations and a tunnel chain was not deleted")
	}
	var gone, kept bool
	err := store.SQL().QueryRowContext(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM cli_client_sessions WHERE user_machine_id=$1)
 AND NOT EXISTS(SELECT 1 FROM device_token_redemptions WHERE grant_id=$2)
 AND NOT EXISTS(SELECT 1 FROM lazy_activations WHERE policy_id=$3)
 AND NOT EXISTS(SELECT 1 FROM tunnel_config_generations WHERE tunnel_id=$4),
 EXISTS(SELECT 1 FROM device_grants WHERE id=$2 AND state='consumed' AND cli_client_session_id IS NULL)`,
		f.machine, "fk"+suffix+"_grant", "fk"+suffix+"_policy", "fk"+suffix+"_tunnel").Scan(&gone, &kept)
	if err != nil || !gone || !kept {
		t.Fatalf("operational rows gone=%v consumed grant kept without session=%v: %v", gone, kept, err)
	}
}

func TestFinalDeleteSkipsMachineThatCannotBeDeleted(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := newFinalDeleteOwner(t, store, suffix)
	first, poison, last := newRemovedMachine(t, store, owner, "a"+suffix), newRemovedMachine(t, store, owner, "b"+suffix), newRemovedMachine(t, store, owner, "c"+suffix)
	// Removal order decides batch order: the failing machine sits between two that must still be deleted.
	first.exec(`UPDATE user_machines SET deleted_at=now()-interval '3 days' WHERE id=$1`, first.machine)
	first.exec(`UPDATE user_machines SET deleted_at=now()-interval '2 days' WHERE id=$1`, poison.machine)
	first.exec(`UPDATE user_machines SET deleted_at=now()-interval '1 day' WHERE id=$1`, last.machine)
	table := "final_delete_blocker_" + suffix
	first.exec(`CREATE TABLE ` + table + `(machine_id text REFERENCES user_machines(id))`)
	t.Cleanup(func() { _, _ = store.SQL().ExecContext(ctx, `DROP TABLE IF EXISTS `+table) })
	first.exec(`INSERT INTO `+table+` VALUES($1)`, poison.machine)

	err := finalDeleteTestService(store).RetryPendingRevocations(ctx)
	if err == nil || !strings.Contains(err.Error(), "skipped 1 removed machines") {
		t.Fatalf("skipped machine not reported: %v", err)
	}
	if first.exists(t) || last.exists(t) || !poison.exists(t) {
		t.Fatalf("first exists=%v poison exists=%v last exists=%v", first.exists(t), poison.exists(t), last.exists(t))
	}
	first.exec(`DROP TABLE ` + table)
	if err := finalDeleteTestService(store).RetryPendingRevocations(ctx); err != nil || poison.exists(t) {
		t.Fatalf("machine not deleted once unblocked: %v", err)
	}
}
