//go:build load

package teams

// F21-170: teams hold their bounds under load, no storm and no slow neighbor. Many teams at the member limit (128) change
// settings at once through Service.Mutate (each takes its team's write bucket, F21-20) while an unrelated small team does the
// same; the neighbor's latency against its latency alone, the failures (none allowed) and the database's idle-in-transaction
// samples are the checks, and the member limit still refuses the next member under load.
//
//	PAPERBOAT_TEST_DATABASE_DSN=... go test -tags load -run TestLoadTeamMutations -v ./internal/teams/
//	(PAPERBOAT_LOAD_TEAMS, PAPERBOAT_LOAD_SECONDS, PAPERBOAT_LOAD_PAUSE_MS: 40 teams of 128 members, 20 s, one change per team per second)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func loadP99(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	return values[min(len(values)-1, len(values)*99/100)]
}

func TestLoadTeamMutations(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	teamsN, seconds := 40, 20
	if v, err := strconv.Atoi(os.Getenv("PAPERBOAT_LOAD_TEAMS")); err == nil && v > 0 {
		teamsN = v
	}
	if v, err := strconv.Atoi(os.Getenv("PAPERBOAT_LOAD_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	// A team admin changes a setting about once a second at the very most; PAPERBOAT_LOAD_PAUSE_MS=100 is the saturating profile
	// (188 mutations/s on a 4-vCPU Boat VM: about 1.5% of them exhaust the 10 serialization retries, SQLSTATE 40001).
	pause := time.Second
	if v, err := strconv.Atoi(os.Getenv("PAPERBOAT_LOAD_PAUSE_MS")); err == nil && v > 0 {
		pause = time.Duration(v) * time.Millisecond
	}
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := "ltm_" + run
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()
		for _, q := range []string{`DELETE FROM paperboat.audit_events WHERE team_id LIKE $1`, `DELETE FROM paperboat.team_members WHERE team_id LIKE $1`, `DELETE FROM paperboat.billing_workspaces WHERE team_id LIKE $1`, `DELETE FROM paperboat.teams WHERE team_id LIKE $1`} {
			_, _ = store.SQL().ExecContext(c, q, prefix+"%")
		}
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.users WHERE id LIKE $1`, prefix+"%")
	})
	// team i has owner prefix_i_o and members prefix_i_m1..m(MaximumMembers-1); the neighbor team has only its owner.
	seed := func(team string, members int) string {
		owner := team + "_o"
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, owner)
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
		exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',200,now()-interval '1 hour',now()+interval '1 day')`, "bw_"+team, team)
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)
		if members > 1 {
			exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT $1||'_m'||g,$1||'_m'||g,$1||'_m'||g||'@example.test','active' FROM generate_series(1,$2::int) g`, team, members-1)
			exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) SELECT $1,$1||'_m'||g,1,'member',true FROM generate_series(1,$2::int) g`, team, members-1)
		}
		return owner
	}
	service := NewService(store)
	owners := make([]string, teamsN)
	names := make([]string, teamsN)
	for i := range owners {
		names[i] = fmt.Sprintf("%s_t%03d", prefix, i)
		owners[i] = seed(names[i], MaximumMembers)
	}
	neighborTeam := prefix + "_n"
	neighborOwner := seed(neighborTeam, 1)

	generationOf := func(team string) uint64 {
		var g uint64
		if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	neighborGeneration := generationOf(neighborTeam)
	var op atomic.Int64
	var firstProbeError atomic.Value
	probe := func(until time.Time) (latencies []time.Duration, failures int) {
		for time.Now().Before(until) {
			start := time.Now()
			got, err := service.Mutate(ctx, neighborOwner, neighborTeam, MutationRequest{OperationID: fmt.Sprintf("probe_%s_%d", run, op.Add(1)), ExpectedGeneration: neighborGeneration, Action: "rename", Name: fmt.Sprintf("n%d", op.Load())})
			if err != nil {
				failures++
				firstProbeError.CompareAndSwap(nil, err.Error())
			} else {
				neighborGeneration = got.Generation
			}
			latencies = append(latencies, time.Since(start))
			time.Sleep(10 * time.Millisecond)
		}
		return
	}
	base, baseFailures := probe(time.Now().Add(5 * time.Second))
	if baseFailures != 0 {
		t.Fatalf("the neighbor alone failed %d times (first: %v)", baseFailures, firstProbeError.Load())
	}

	var maxIdleInTx, maxLockWait atomic.Int64
	stop := make(chan struct{})
	var sampling sync.WaitGroup
	sampling.Add(1)
	go func() {
		defer sampling.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			var idle, wait int64
			if err := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FILTER (WHERE state='idle in transaction'), count(*) FILTER (WHERE wait_event_type='Lock') FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()`).Scan(&idle, &wait); err == nil {
				maxIdleInTx.Store(max(maxIdleInTx.Load(), idle))
				maxLockWait.Store(max(maxLockWait.Load(), wait))
			}
		}
	}()
	var mutations, failuresTotal atomic.Int64
	var firstError atomic.Value
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	var load sync.WaitGroup
	for i := range owners {
		load.Add(1)
		go func() {
			defer load.Done()
			generation := generationOf(names[i])
			for seq := 0; time.Now().Before(deadline); seq++ {
				got, err := service.Mutate(ctx, owners[i], names[i], MutationRequest{OperationID: fmt.Sprintf("load_%s_%d_%d", run, i, seq), ExpectedGeneration: generation, Action: "rename", Name: fmt.Sprintf("team %d.%d", i, seq)})
				if err != nil {
					failuresTotal.Add(1)
					firstError.CompareAndSwap(nil, err.Error())
					continue
				}
				generation = got.Generation
				mutations.Add(1)
				time.Sleep(pause)
			}
		}()
	}
	loaded, failures := probe(deadline)
	load.Wait()
	close(stop)
	sampling.Wait()
	t.Logf("%d teams of %d members, %d s: %d mutations (%.1f/s), %d failures (first: %v)", teamsN, MaximumMembers, seconds, mutations.Load(), float64(mutations.Load())/float64(seconds), failuresTotal.Load(), firstError.Load())
	t.Logf("neighbor alone p99 %v; under load p99 %v (%d samples), %d failures; database: max idle-in-transaction %d, max sessions waiting on a lock %d", loadP99(base), loadP99(loaded), len(loaded), failures, maxIdleInTx.Load(), maxLockWait.Load())
	if failuresTotal.Load() != 0 || failures != 0 {
		t.Errorf("failures under load: teams %d (%v), neighbor %d", failuresTotal.Load(), firstError.Load(), failures)
	}
	if got, bound := loadP99(loaded), max(20*loadP99(base), time.Second); got > bound {
		t.Errorf("slow neighbor: p99 %v under load against %v alone (bound %v)", got, loadP99(base), bound)
	}
	if int(maxIdleInTx.Load()) > teamsN {
		t.Errorf("idle-in-transaction sessions: %d for %d teams", maxIdleInTx.Load(), teamsN)
	}
	// The bound still holds under load: a team at the member limit refuses the next member.
	err = store.InTx(ctx, func(c context.Context, tx *db.Tx) error { return capacity(c, tx, "someone-new", names[0]) })
	if !errors.Is(err, ErrLimit) {
		t.Errorf("a team at %d members admitted another: %v", MaximumMembers, err)
	}
}
