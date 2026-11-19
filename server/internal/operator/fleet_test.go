package operator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestFleetVersionOrderAndOldestOnline(t *testing.T) {
	a, _ := versionKey("2026.10.9.3")
	b, _ := versionKey("2026.10.10.1")
	if !versionLess(a, b) || versionLess(b, a) || versionLess(a, a) {
		t.Fatalf("2026.10.9.3 must be older than 2026.10.10.1 (numeric, not text, order)")
	}
	for _, v := range []string{"dev-abc1234", "unknown", "2026.10.9", "2026.10.x.1", "-1.2.3.4", ""} {
		if _, ok := versionKey(v); ok {
			t.Errorf("%q has no order", v)
		}
	}
	rows := []FleetVersionRow{
		{Platform: "linux", Version: "2026.10.9.3", Online: 0},
		{Platform: "linux", Version: "2026.10.12.1", Online: 2},
		{Platform: "darwin", Version: "2026.10.10.1", Online: 1},
		{Platform: "linux", Version: "dev-abc", Online: 5},
	}
	if got := oldestOnline(rows, ""); got != "2026.10.10.1" {
		t.Errorf("oldest online = %q (offline-only and dev versions are skipped)", got)
	}
	if got := oldestOnline(rows, "linux"); got != "2026.10.12.1" {
		t.Errorf("oldest online linux = %q", got)
	}
}

func TestFleetKindsAreOperatorOnly(t *testing.T) {
	for _, kind := range []string{"fleet_versions", "fleet_rollouts", "fleet_offline"} {
		for role, want := range map[string]bool{"admin": true, "operations": true, "support": false, "billing": false} {
			if got := allowed(auth.Role(role), kind); got != want {
				t.Errorf("%s may read %s: %v, want %v", role, kind, got, want)
			}
		}
		if !pageKind(kind) {
			t.Errorf("%s must accept an offset", kind)
		}
	}
}

// fleetSeed builds a small fleet under one fresh account and returns the service and a unique suffix. Versions use
// the year 2098/2099 so every exact assertion is about rows only this test creates.
func fleetSeed(t *testing.T) (*Service, *db.DB, string) {
	t.Helper()
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
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	user := "usr_fleet_" + suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`INSERT INTO users(id,workos_subject,primary_email,display_name,status,role) VALUES($1,$1,$2,'Fleet','active','user')`, user, user+"@example.test")
	t.Cleanup(func() {
		bg := context.Background()
		store.Pool().Exec(bg, `DELETE FROM user_machines WHERE user_id=$1`, user)
		store.Pool().Exec(bg, `DELETE FROM users WHERE id=$1`, user)
		store.Pool().Exec(bg, `DELETE FROM release_authority_bundles WHERE release_id=$1`, "rel_fleet_"+suffix)
		store.Pool().Exec(bg, `DELETE FROM release_authority_requests WHERE release_id=$1`, "rel_fleet_"+suffix)
		store.Pool().Exec(bg, `DELETE FROM fleet_version_daily WHERE version LIKE '2098.%' OR version LIKE '2099.%'`)
	})
	// alias, platform, reporter version, last seen (an interval; "" never), online
	for i, m := range [][5]string{
		{"new", "linux", "2099.2.1.1", "0 seconds", "t"},
		{"never", "linux", "2098.1.1.1", "", "f"},
		{"hours", "linux", "2098.1.1.1", "2 hours", "f"},
		{"failed", "linux", "2098.1.1.1", "10 days", "f"},
		{"mac", "darwin", "2098.1.1.1", "40 days", "f"},
		{"dev", "linux", "", "3 days", "f"},
	} {
		id := fmt.Sprintf("host-fleet-%d-%s", i, suffix)
		state := "offline"
		if m[4] == "t" {
			state = "online"
		}
		seen := "NULL"
		if m[3] != "" {
			seen = "now()-interval '" + m[3] + "'"
		}
		exec(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,alias,reporter_version,online,last_seen_at)
 VALUES($1,$2,$1,$3,'amd64','/workspace',$4,'occupied',$5,NULLIF($6,''),$7::boolean,`+seen+`)`, id, user, m[1], state, m[0]+"-"+suffix, m[2], m[4])
	}
	obs := func(alias, current, target, state, code string) {
		exec(`INSERT INTO user_machine_update_observations(user_machine_id,environment_id,schema,current_version,target_version,channel,state,error_code,operation_id,installation_generation,worker_generation,os_boot_id,rollback_count,observed_at,payload_hash)
 SELECT id,environment_id,'test.fleet/v1',$2,NULLIF($3,''),'stable',$4,NULLIF($5,''),'operation-'||id,1,1,'boot',0,now(),decode(repeat('00',32),'hex') FROM user_machines WHERE user_id=$1 AND alias=$6`,
			user, current, target, state, code, alias+"-"+suffix)
	}
	obs("failed", "2098.1.1.1", "2099.2.1.1", "failed", "signature_invalid")
	obs("dev", "dev-abc1234", "", "idle", "")
	for i, id := range []string{"req_fleet_1_" + suffix, "req_fleet_2_" + suffix} {
		exec(`INSERT INTO release_authority_requests(id,action,release_id,version,platform,architecture,policy_revision,rollout_percentage,requested_by_user_id,idempotency_key,request_hash)
 VALUES($1,'promote',$2,'2099.2.1.1','linux','amd64',$3,25,$4,$1,decode(repeat($5::text,32),'hex'))`, id, "rel_fleet_"+suffix, i+1, user, fmt.Sprintf("0%d", i+1))
	}
	exec(`INSERT INTO release_authority_bundles(id,release_id,version,platform,architecture,action,policy_revision,payload,payload_hash,signatures,issued_at,expires_at,authority_request_id,imported_by_user_id)
 VALUES($1,$2,'2099.2.1.1','linux','amd64','promote',1,'{"rollout_percentage":25}',decode(repeat('01',32),'hex'),'[]',now()-interval '5 days',now()+interval '30 days',$5,$4),
 ($3,$2,'2099.2.1.1','linux','amd64','promote',2,'{"rollout_percentage":100}',decode(repeat('02',32),'hex'),'[]',now()-interval '1 day',now()+interval '30 days',$6,$4)`,
		"rab_fleet_1_"+suffix, "rel_fleet_"+suffix, "rab_fleet_2_"+suffix, user, "req_fleet_1_"+suffix, "req_fleet_2_"+suffix)
	exec(`INSERT INTO fleet_version_daily(day,platform,version,machines) VALUES((now() AT TIME ZONE 'UTC')::date-3,'linux','2098.1.1.1',10),((now() AT TIME ZONE 'UTC')::date-3,'linux','2099.2.1.1',0)`)
	return NewService(store), store, suffix
}

func TestFleetVersionsRolloutsAndOfflineOnPostgres(t *testing.T) {
	s, store, suffix := fleetSeed(t)
	ctx := context.Background()
	if err := s.SnapshotFleet(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotFleet(ctx); err != nil { // a second snapshot replaces today's rows, never doubles them
		t.Fatal(err)
	}
	err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		v, err := s.readFleetVersions(ctx, tx)
		if err != nil {
			return err
		}
		row := func(platform, version string) FleetVersionRow {
			for _, r := range v.Versions {
				if r.Platform == platform && r.Version == version {
					return r
				}
			}
			return FleetVersionRow{}
		}
		if r := row("linux", "2099.2.1.1"); r.Machines != 1 || r.Online != 1 || r.Users != 1 {
			t.Errorf("linux 2099.2.1.1 = %+v", r)
		}
		if r := row("linux", "2098.1.1.1"); r.Machines != 3 || r.Online != 0 || r.Users != 1 {
			t.Errorf("linux 2098.1.1.1 = %+v", r)
		}
		if r := row("darwin", "2098.1.1.1"); r.Machines != 1 {
			t.Errorf("darwin 2098.1.1.1 = %+v", r)
		}
		if r := row("linux", "dev-abc1234"); r.Machines != 1 {
			t.Errorf("a machine with no reporter version falls back to its update agent's: %+v", r)
		}
		var found bool
		for _, f := range v.Failures {
			found = found || f == FleetFailure{TargetVersion: "2099.2.1.1", State: "failed", ErrorCode: "signature_invalid", Machines: 1}
		}
		if !found {
			t.Errorf("failure reasons = %+v", v.Failures)
		}

		ro, err := s.readFleetRollouts(ctx, tx)
		if err != nil {
			return err
		}
		var r *FleetRollout
		for i := range ro.Rollouts {
			if ro.Rollouts[i].Version == "2099.2.1.1" {
				r = &ro.Rollouts[i]
			}
		}
		if r == nil {
			t.Fatalf("rollout missing: %+v", ro)
		}
		if r.Updated != 1 || r.Failed != 1 || r.RolledBack != 0 || r.Older < 3 || r.UnreachableSince < 2 || r.OfflineNow < 1 || r.Other < 1 {
			t.Errorf("rollout counts = %+v", *r)
		}
		if r.Action != "promote" || r.RolloutPercentage != 100 || len(r.Platforms) != 1 || r.Platforms[0] != "linux" {
			t.Errorf("rollout decision = %s %d%% %v (the latest promote, over the platforms it covers)", r.Action, r.RolloutPercentage, r.Platforms)
		}
		if time.Since(r.StartedAt) < 4*24*time.Hour || time.Since(r.StartedAt) > 6*24*time.Hour {
			t.Errorf("a rollout starts at its first promote: %v", r.StartedAt)
		}
		if len(r.Failures) != 1 || r.Failures[0].ErrorCode != "signature_invalid" {
			t.Errorf("rollout failures = %+v", r.Failures)
		}
		if len(r.Series) < 2 || r.Series[0].Total != 10 || r.Series[0].Updated != 0 {
			t.Fatalf("series = %+v (the day three days ago holds 10 older machines)", r.Series)
		}
		last := r.Series[len(r.Series)-1]
		if last.Updated != 1 || last.Total < 4 || last.Failed != 1 {
			t.Errorf("today's point = %+v", last)
		}

		off, err := s.readFleetOffline(ctx, tx, "q="+suffix, "", 2)
		if err != nil {
			return err
		}
		if off.Total != 5 || len(off.Items) != 2 || off.NextAfter != "2" || off.Items[0].Alias != "never-"+suffix || off.Items[0].Bucket != "never_seen" || off.Items[1].Bucket != "over_30_days" {
			t.Errorf("first page = total %d next %q %+v (longest unseen first, the never-seen machine leading)", off.Total, off.NextAfter, off.Items)
		}
		page3, err := s.readFleetOffline(ctx, tx, "q="+suffix, "4", 2)
		if err != nil {
			return err
		}
		if len(page3.Items) != 1 || page3.NextAfter != "" || page3.Items[0].Alias != "hours-"+suffix || page3.Items[0].Version != "2098.1.1.1" || page3.Items[0].LastSeenAt == nil {
			t.Errorf("last page = %+v next %q", page3.Items, page3.NextAfter)
		}
		linuxOld, err := s.readFleetOffline(ctx, tx, "q="+suffix+"&bucket=7_to_30_days&platform=linux", "", 50)
		if err != nil {
			return err
		}
		if linuxOld.Total != 1 || linuxOld.Items[0].Alias != "failed-"+suffix {
			t.Errorf("bucket filter = %+v", linuxOld)
		}
		byBucket := map[string]int64{}
		for _, b := range off.Buckets {
			byBucket[b.Bucket] = b.Machines
		}
		if byBucket["under_1_day"] < 1 || byBucket["1_to_7_days"] < 1 || byBucket["7_to_30_days"] < 1 || byBucket["over_30_days"] < 1 || byBucket["never_seen"] < 1 || off.Offline < 5 || off.Machines < 6 {
			t.Errorf("fleet buckets = %+v offline %d of %d", off.Buckets, off.Offline, off.Machines)
		}
		if _, err := s.readFleetOffline(ctx, tx, "bucket=bogus", "", 10); !errors.Is(err, ErrInvalid) {
			t.Errorf("unknown bucket: %v", err)
		}
		if _, err := s.readFleetOffline(ctx, tx, "", "-1", 10); !errors.Is(err, ErrInvalid) {
			t.Errorf("negative offset: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
