package retention

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Team inbox requests (and their receipt outbox rows, by cascade) were kept forever. One pass deletes ended requests
// and undecided ones past the window, and keeps recent and in-flight ones.
func TestRetentionBoundsTeamInboxOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	x := fmt.Sprint(time.Now().UnixNano())
	sender, recipient, team, src, dst := "ri_s_"+x, "ri_r_"+x, "ri-team-"+x, "ri_src_"+x, "ri_dst_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_inbox_requests WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machines WHERE id IN ($1,$2)`, src, dst)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, sender, recipient)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, sender, recipient)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'inbox retention')`, team, sender)
	for id, owner := range map[string]string{src: sender, dst: recipient} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'m-'||md5($1),'linux','amd64','/workspace','online','occupied',true,1)`, id, owner)
	}
	// name, status, updated N days ago, expires N days from now (negative: ago); kept says whether a pass must keep it.
	cases := []struct {
		name, status         string
		updatedAgo, expireIn int
		kept                 bool
	}{
		{"completed_old", "completed", 40, -40, false}, {"declined_old", "declined", 40, -40, false},
		{"expired_old", "expired", 40, -40, false}, {"revoked_old", "revoked", 40, -40, false}, {"consumed_old", "consumed", 40, -40, false},
		{"pending_lapsed_old", "pending", 50, -40, false}, {"approved_lapsed_old", "approved", 50, -40, false},
		{"completed_recent", "completed", 1, -1, true}, {"pending_lapsed_recent", "pending", 5, -1, true},
		{"pending_live", "pending", 1, 1, true}, {"approved_live", "approved", 40, 1, true}, {"consumed_recent", "consumed", 1, -1, true},
	}
	for i, c := range cases {
		id := fmt.Sprintf("ri_%s_%s", c.name, x)
		exec(`INSERT INTO paperboat.team_inbox_requests(request_id,operation_id,team_id,sender_account,recipient_account,source_machine_id,destination_machine_id,batch_id,manifest_digest,manifest,status,decision_kind,policy_generation,expires_at,created_at,updated_at)
 VALUES($1,$1,$2,$3,$4,$5,$6,$1,repeat('a',64),'[{"name":"f","size":1}]',$7,'manual',1,now()+make_interval(days=>$8),now()-make_interval(days=>$9),now()-make_interval(days=>$9))`,
			id, team, sender, recipient, src, dst, c.status, c.expireIn, c.updatedAgo)
		if i == 0 || c.name == "pending_live" {
			// A failed outbox row never retried again: gone with its old request, kept with its live one.
			exec(`INSERT INTO paperboat.team_inbox_receipt_outbox(request_id,recipient_account,status,attempts,next_attempt_at) VALUES($1,$2,'failed',5,now()+interval '100 years')`, id, recipient)
		}
	}
	if _, err := Run(ctx, store, config.Default().Retention); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		var n int
		id := fmt.Sprintf("ri_%s_%s", c.name, x)
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_inbox_requests WHERE request_id=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (n == 1) != c.kept {
			t.Errorf("%s: %d rows left, kept=%v want %v", c.name, n, n == 1, c.kept)
		}
	}
	var outbox int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_inbox_receipt_outbox WHERE request_id IN ($1,$2)`, "ri_completed_old_"+x, "ri_pending_live_"+x).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox rows left = %d (%v), want only the live request's", outbox, err)
	}
}
