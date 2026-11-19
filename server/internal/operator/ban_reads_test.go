package operator

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Accounts and teams show a running ban with its reference, the status filters find them, an admin sees an account's recent
// sign-in addresses (with whether a running block covers each), and the IP blocks list pages through running and ended ones;
// nobody below admin reads addresses or blocks.
func TestBanFieldsRecentAddressesAndBlockList(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) // registered first, so it runs after the cleanups that use the store
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	opID, supportID, bannedID, freeID, team := "usr_bn_op_"+suffix, "usr_bn_sp_"+suffix, "usr_bn_b_"+suffix, "usr_bn_f_"+suffix, "bn-team-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@example.test','x','active','admin'),($2,$2||'@example.test','x','active','support')`, opID, supportID)
	exec(`INSERT INTO users(id,workos_subject,primary_email,display_name,status,role) VALUES($1,$1,$1||'@example.test','Bn Banned','suspended','user'),($2,$2,$2||'@example.test','Bn Free','active','user')`, bannedID, freeID)
	exec(`INSERT INTO teams(team_id,owner_account,generation,suspended,suspended_at,suspended_by) VALUES($1,$2,2,true,now(),$2)`, team, bannedID)
	exec(`INSERT INTO bans(id,subject_kind,subject_id,reason,banned_by) VALUES('ban_u_'||$1,'user',$2,'ABUSE-17','operator_x'),('ban_t_'||$1,'team',$3,'ABUSE-17','operator_x')`, suffix, bannedID, team)
	for i, ip := range []string{"203.0.113.9", "198.51.100.4", "203.0.113.9"} {
		exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,client_ip,created_at) VALUES($1,$2,'user','auth.login','user',$2,$3,now()-($4||' minutes')::interval)`, "aud_bn_"+suffix+string(rune('a'+i)), bannedID, ip, fmt.Sprint(10-i))
	}
	exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,client_ip) VALUES($1,$2,'user','auth.logout','user',$2,'192.0.2.50')`, "aud_bn_out_"+suffix, bannedID)
	for i, kind := range []string{"auth.device.completed", "auth.client.approved", "auth.client.api_token", "auth.client.recipe"} {
		exec(`INSERT INTO audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,client_ip,created_at) VALUES($1,$2,'user',$3,'client_session','s',$4,now()-interval '20 minutes')`, fmt.Sprintf("aud_bn_k%d_%s", i, suffix), bannedID, kind, fmt.Sprintf("192.0.2.%d", 100+i))
	}
	exec(`INSERT INTO ip_blocks(id,cidr,reason,created_by,expires_at,revoked_at,revoked_by) VALUES
 ('ipb_bn1_'||$1,'203.0.113.0/24','blk:'||$1,'operator_x',NULL,NULL,NULL),
 ('ipb_bn2_'||$1,'2001:db8:bb::/48','blk:'||$1,'operator_x',now()+interval '1 day',NULL,NULL),
 ('ipb_bn3_'||$1,'192.0.2.0/24','blk:'||$1,'operator_x',NULL,now(),'operator_x'),
 ('ipb_bn4_'||$1,'192.0.3.0/24','blk:'||$1,'operator_x',now()-interval '1 hour',NULL,NULL)`, suffix)
	t.Cleanup(func() {
		bg := context.Background()
		store.Pool().Exec(bg, `DELETE FROM ip_blocks WHERE reason=$1`, "blk:"+suffix)
		store.Pool().Exec(bg, `DELETE FROM bans WHERE subject_id IN ($1,$2)`, bannedID, team)
		store.Pool().Exec(bg, `DELETE FROM teams WHERE team_id=$1`, team)
		store.Pool().Exec(bg, `DELETE FROM users WHERE id IN ($1,$2)`, bannedID, freeID)
		store.Pool().Exec(bg, `DELETE FROM operator_accounts WHERE id IN ($1,$2)`, opID, supportID)
	})
	service := NewService(store)
	admin, support := auth.User{ID: opID, Role: auth.Role("admin")}, auth.User{ID: supportID, Role: auth.Role("support")}
	filter := func(kv ...string) string {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v.Encode()
	}

	v, err := service.Read(ctx, support, "accounts", filter("q", "usr_bn_", "status", "suspended"), "", 10)
	if err != nil || len(v.(AccountPage).Accounts) != 1 || v.(AccountPage).Accounts[0].ID != bannedID || v.(AccountPage).Accounts[0].BanReason != "ABUSE-17" || v.(AccountPage).Accounts[0].BannedAt == nil {
		t.Fatalf("the banned account in the list: %+v %v", v, err)
	}
	if v, err = service.Read(ctx, support, "account", bannedID, "", 10); err != nil || v.(Account).BanReason != "ABUSE-17" {
		t.Fatalf("account detail: %+v %v", v, err)
	}
	if v, err = service.Read(ctx, support, "account", freeID, "", 10); err != nil || v.(Account).BanReason != "" || v.(Account).BannedAt != nil {
		t.Fatalf("an account without a ban: %+v %v", v, err)
	}
	if v, err = service.Read(ctx, support, "teams", filter("q", team, "suspended", "true"), "", 10); err != nil || len(v.(TeamPage).Teams) != 1 || v.(TeamPage).Teams[0].BanReason != "ABUSE-17" {
		t.Fatalf("the banned team in the list: %+v %v", v, err)
	}
	if v, err = service.Read(ctx, support, "team", team, "", 10); err != nil || v.(Team).BanReason != "ABUSE-17" {
		t.Fatalf("team detail: %+v %v", v, err)
	}

	// Recent addresses: grouped, newest first, only sign-in kinds, with the covering block marked; admin only.
	v, err = service.Read(ctx, admin, "account_ips", bannedID, "", 10)
	ips := v.(AccountIPs).IPs
	if err != nil || len(ips) != 6 || ips[0].IP != "203.0.113.9" || ips[0].Count != 2 || !ips[0].Blocked || ips[1].IP != "198.51.100.4" || ips[1].Blocked {
		t.Fatalf("recent addresses: %+v %v", v, err)
	}
	for _, kind := range []string{"account_ips", "ip_blocks"} {
		if _, err = service.Read(ctx, support, kind, "", "", 10); err == nil {
			t.Fatalf("a support operator read %s", kind)
		}
	}
	if _, err = service.Read(ctx, admin, "account_ips", "usr_missing_"+suffix, "", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recent addresses of a missing account: %v", err)
	}

	// The block list: newest first, paged, with the running/ended filter.
	page, err := service.Read(ctx, admin, "ip_blocks", "", "", 2)
	first := page.(IPBlockPage)
	if err != nil || len(first.Blocks) != 2 || first.NextAfter == "" || first.Total < 4 {
		t.Fatalf("first page: %+v %v", first, err)
	}
	rest, err := service.Read(ctx, admin, "ip_blocks", "", first.NextAfter, 100)
	all := append(first.Blocks, rest.(IPBlockPage).Blocks...)
	if err != nil || rest.(IPBlockPage).NextAfter != "" {
		t.Fatalf("second page: %+v %v", rest, err)
	}
	ours := map[string]bool{}
	for _, b := range all {
		if strings.HasPrefix(b.ID, "ipb_bn") {
			ours[b.ID[:8]] = b.Running
		}
	}
	if len(ours) != 4 || !ours["ipb_bn1_"] || !ours["ipb_bn2_"] || ours["ipb_bn3_"] || ours["ipb_bn4_"] {
		t.Fatalf("running flags %v", ours)
	}
	running, err := service.Read(ctx, admin, "ip_blocks", "running", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range running.(IPBlockPage).Blocks {
		if !b.Running {
			t.Fatalf("an ended block in the running list: %+v", b)
		}
	}
	ended, err := service.Read(ctx, admin, "ip_blocks", "ended", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range ended.(IPBlockPage).Blocks {
		if b.Running {
			t.Fatalf("a running block in the ended list: %+v", b)
		}
	}
	if _, err = service.Read(ctx, admin, "ip_blocks", "weird", "", 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown state: %v", err)
	}
}
