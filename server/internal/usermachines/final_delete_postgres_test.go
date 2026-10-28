package usermachines

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/activity"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
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
