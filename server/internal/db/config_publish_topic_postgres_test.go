package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A new published revision wakes the config worker of every other machine pulling from that repository, once per
// revision: not the publisher, not a push-only or revoked machine, not a machine on another repository.
func TestConfigPublishMovesConfigTopicOfPullingMachinesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	user, repo, other := "cp_user_"+s, "cp_repo_"+s, "cp_other_"+s
	publisher, puller, pushOnly, revoked, elsewhere := "cp_pub_"+s, "cp_pull_"+s, "cp_push_"+s, "cp_rev_"+s, "cp_else_"+s
	machines := []string{publisher, puller, pushOnly, revoked, elsewhere}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) SELECT m,m,$1 FROM unnest($2::text[]) m`, user, machines)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state)
SELECT m,$1,m,'m-'||n,'linux','amd64','/w','online','occupied' FROM unnest($2::text[]) WITH ORDINALITY AS t(m,n)`, user, machines)
	exec(`INSERT INTO paperboat.control_config_repositories(id,owner_user_id,provider,external_ref,display_name) VALUES($1,$3,'github',$1,'a'),($2,$3,'github',$2,'b')`, repo, other, user)
	exec(`INSERT INTO paperboat.control_config_assignments(id,machine_id,environment_id,repository_id,mode,revoked_at) VALUES
($1,$1,$1,$6,'bidirectional',NULL),($2,$2,$2,$6,'pull_only',NULL),($3,$3,$3,$6,'push_only',NULL),($4,$4,$4,$6,'pull_only',now()),($5,$5,$5,$7,'pull_only',NULL)`,
		publisher, puller, pushOnly, revoked, elsewhere, repo, other)
	generations := func() map[string]int64 {
		t.Helper()
		out := map[string]int64{}
		for _, m := range machines {
			var n int64
			if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(max(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='config'`, m).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[m] = n
		}
		return out
	}
	report := func(revision any) {
		t.Helper()
		exec(`INSERT INTO paperboat.control_config_sync_statuses(environment_id,repository_id,assignment_id,machine_id,installation_generation,warning_revision,policy_revision,sync_revision,state,machine_updated_at,last_published_revision)
VALUES($1,$2,$1,$1,1,'w','p',1,'healthy',now(),$3)
ON CONFLICT (environment_id) DO UPDATE SET last_published_revision=EXCLUDED.last_published_revision, observed_at=now()`, publisher, repo, revision)
	}
	expect := func(step string, before map[string]int64, moved ...string) {
		t.Helper()
		after := generations()
		want := map[string]bool{}
		for _, m := range moved {
			want[m] = true
		}
		for _, m := range machines {
			if got := after[m] > before[m]; got != want[m] {
				t.Fatalf("%s: machine %s moved=%v, want %v", step, m, got, want[m])
			}
		}
	}
	before := generations()
	report(nil)
	expect("status without a publish", before)
	before = generations()
	report("rev1")
	expect("first publish", before, puller)
	before = generations()
	report("rev1")
	expect("same revision reported again", before)
	before = generations()
	report("rev2")
	expect("next publish", before, puller)
}
