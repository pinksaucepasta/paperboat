package bandwidthgate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func gateStore(t *testing.T) *db.DB {
	t.Helper()
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
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

// gateFixture is one account with a personal and a team workspace and their usage state.
type gateFixture struct {
	t                *testing.T
	store            *db.DB
	user, team       string
	personal, teamWS string
}

func newGateFixture(t *testing.T, store *db.DB) *gateFixture {
	t.Helper()
	x := fmt.Sprint(time.Now().UnixNano())
	f := &gateFixture{t: t, store: store, user: "relay_quota_u_" + x, team: "relay-quota-t-" + x}
	f.personal, f.teamWS = "personal:"+f.user, "team:"+f.team
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, f.user)
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, f.team, f.user)
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, f.team, f.user)
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,state) VALUES($1,$2,'free')`, f.personal, f.user)
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state) VALUES($1,$2,'free')`, f.teamWS, f.team)
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM paperboat.billing_workspace_topups WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM paperboat.billing_workspace_operations WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM paperboat.billing_workspaces WHERE id IN ($1,$2)`,
		} {
			_, _ = store.SQL().ExecContext(bg, q, f.personal, f.teamWS)
		}
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, f.team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, f.team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, f.user)
	})
	return f
}

func (f *gateFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gateFixture) period(workspace string, included, consumed int64, paid bool) {
	f.t.Helper()
	now := time.Now().UTC()
	f.exec(`INSERT INTO paperboat.billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,paid,budget_start) VALUES($1,$2,$3,$4,$5,$6,$2)`, workspace, now.Add(-time.Hour), now.AddDate(0, 1, 0), included, consumed, paid)
}

func (f *gateFixture) clearPeriods() {
	f.exec(`DELETE FROM paperboat.billing_workspace_usage_periods WHERE workspace_id IN ($1,$2)`, f.personal, f.teamWS)
	f.exec(`DELETE FROM paperboat.billing_workspace_topups WHERE workspace_id IN ($1,$2)`, f.personal, f.teamWS)
}

func (f *gateFixture) topup(workspace string, remaining int64) {
	f.t.Helper()
	key := fmt.Sprint(time.Now().UnixNano())
	f.exec(`INSERT INTO paperboat.billing_workspace_operations(idempotency_key,workspace_id,actor_user_id,kind,request_hash,state) VALUES($1,$2,$3,'topup','fixture','succeeded')`, key, workspace, f.user)
	f.exec(`INSERT INTO paperboat.billing_workspace_topups(provider_session_id,operation_key,workspace_id,purchased_bytes,remaining_bytes,purchased_at) VALUES($1,$1,$2,10,$3,$4)`, key, workspace, remaining, time.Now().UTC().Add(-time.Minute))
}

// F13-03: bandwidth is refused only when every workspace the account can use is out of
// allowance, judged from stored usage by one read-only statement.
func TestGateJudgesEveryWorkspacePostgres(t *testing.T) {
	store := gateStore(t)
	f := newGateFixture(t, store)
	gate := New(store)
	denied := func() bool {
		t.Helper()
		got, err := gate.Lookup(context.Background(), f.user, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if denied() {
		t.Fatal("denied before any usage period exists")
	}
	f.period(f.personal, 100, 100, false)
	if denied() {
		t.Fatal("denied although the team workspace has no usage period yet")
	}
	f.period(f.teamWS, 100, 40, false)
	if denied() {
		t.Fatal("denied while the team workspace still has allowance")
	}
	f.exec(`UPDATE paperboat.billing_workspace_usage_periods SET consumed_bytes=100 WHERE workspace_id=$1`, f.teamWS)
	if !denied() {
		t.Fatal("not denied although every workspace is exhausted")
	}
	f.clearPeriods()
	f.period(f.personal, 100, 100, true)
	f.period(f.teamWS, 100, 100, false)
	f.topup(f.personal, 5)
	if denied() {
		t.Fatal("denied although the paid workspace still has top-up credit")
	}
	f.exec(`UPDATE paperboat.billing_workspace_topups SET remaining_bytes=0 WHERE workspace_id=$1`, f.personal)
	if !denied() {
		t.Fatal("not denied after the last top-up credit was used")
	}
	f.exec(`UPDATE paperboat.billing_workspace_usage_periods SET period_start=now()-interval '2 days',period_end=now()-interval '1 day' WHERE workspace_id=$1`, f.personal)
	if denied() {
		t.Fatal("an expired usage period counted as exhausted")
	}
}

// F13-03 load: the gate sits on the hot peer-network configuration path, so it must not queue
// behind usage-debit transactions. Another connection holds FOR UPDATE on every billing row of
// the account (as a debit does) while 64 callers hammer the gate and the grant query.
func TestGateNeverBlocksBehindUsageDebitsPostgres(t *testing.T) {
	store := gateStore(t)
	f := newGateFixture(t, store)
	f.period(f.personal, 100, 100, true)
	f.period(f.teamWS, 100, 100, false)
	f.topup(f.personal, 0)
	gate := New(store)
	// run drives the gate from many callers and returns every latency.
	run := func() []time.Duration {
		const callers, calls = 32, 6
		latencies := make([]time.Duration, 0, callers*calls)
		problems := map[string]int{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for c := 0; c < callers; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < calls; i++ {
					began := time.Now()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					denied, err := gate.Lookup(ctx, f.user, time.Now().UTC())
					problem := ""
					if err == nil && !denied {
						problem = "wrong answer: exhausted account not denied"
					}
					cancel()
					if err != nil {
						problem = err.Error()
					}
					mu.Lock()
					if problem != "" {
						problems[problem]++
					}
					latencies = append(latencies, time.Since(began))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(problems) != 0 {
			t.Fatalf("calls failed or answered wrongly: %v", problems)
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		return latencies
	}
	quantile := func(l []time.Duration, q int) time.Duration { return l[len(l)*q/100] }

	baseline := run()
	holder, err := store.SQL().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	for _, q := range []string{
		`SELECT id FROM paperboat.billing_workspaces WHERE id IN ($1,$2) FOR UPDATE`,
		`SELECT workspace_id FROM paperboat.billing_workspace_usage_periods WHERE workspace_id IN ($1,$2) FOR UPDATE`,
		`SELECT workspace_id FROM paperboat.billing_workspace_topups WHERE workspace_id IN ($1,$2) FOR UPDATE`,
	} {
		rows, err := holder.Query(q, f.personal, f.teamWS)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		rows.Close()
	}
	locked := run()
	t.Logf("%d calls each: no locks p50 %v p99 %v; FOR UPDATE held on every billing row p50 %v p99 %v",
		len(baseline), quantile(baseline, 50), quantile(baseline, 99), quantile(locked, 50), quantile(locked, 99))
	// A statement that queued behind the held locks would wait its whole 5 s deadline and fail
	// above; the comparison also rules out a smaller, silent slowdown.
	if quantile(locked, 50) > 2*quantile(baseline, 50)+200*time.Millisecond || quantile(locked, 99) > 2*quantile(baseline, 99)+time.Second {
		t.Fatalf("held row locks slowed the gate: baseline p50/p99 %v/%v, locked %v/%v", quantile(baseline, 50), quantile(baseline, 99), quantile(locked, 50), quantile(locked, 99))
	}
}

// The cache bounds database load and the gate fails open on any error.
func TestGateCachesAndFailsOpen(t *testing.T) {
	var calls atomic.Int32
	failing := atomic.Bool{}
	gate := &Gate{timeout: time.Second, lookup: func(context.Context, string, time.Time) (bool, error) {
		calls.Add(1)
		if failing.Load() {
			return false, errors.New("database unavailable")
		}
		return true, nil
	}}
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		if !gate.Denied(ctx, "account_a", time.Now()) {
			t.Fatal("exhausted account was not withheld")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("1000 calls made %d database lookups, want 1", calls.Load())
	}
	if gate.Denied(ctx, "", time.Now()) || (*Gate)(nil).Denied(ctx, "account_a", time.Now()) {
		t.Fatal("empty account or unconfigured gate withheld grants")
	}
	failing.Store(true)
	if gate.Denied(ctx, "account_b", time.Now()) {
		t.Fatal("a failing lookup withheld grants")
	}
	before := calls.Load()
	for i := 0; i < 50; i++ {
		gate.Denied(ctx, "account_b", time.Now())
	}
	if calls.Load() != before {
		t.Fatal("a failure was retried on every call instead of being cached briefly")
	}
	gate.mu.Lock()
	gate.cache["account_a"] = entry{denied: true, until: time.Now().Add(-time.Second)}
	gate.mu.Unlock()
	failing.Store(false)
	gate.Denied(ctx, "account_a", time.Now())
	if calls.Load() != before+1 {
		t.Fatal("an expired cache entry was not refreshed")
	}
}

// A dead database pool is a billing failure like any other: the gate answers 'not denied'
// within its timeout rather than blocking or erroring the configuration.
func TestGateFailsOpenOnAClosedPoolPostgres(t *testing.T) {
	store := gateStore(t)
	dead, err := db.Open(config.Database{Driver: "postgres", DSN: os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	_ = dead.Close()
	_ = store
	gate := New(dead)
	began := time.Now()
	if gate.Denied(context.Background(), "account_dead", time.Now()) {
		t.Fatal("a closed pool withheld relay grants")
	}
	if time.Since(began) > 2*time.Second {
		t.Fatalf("a closed pool stalled the gate for %v", time.Since(began))
	}
}

// A relay observation can name hundreds of accounts: only maxFresh uncached lookups run per
// round, concurrently, and the rest are answered on later rounds from the cache.
func TestGateDeniedAmongBoundsFreshLookups(t *testing.T) {
	var lookups atomic.Int32
	gate := &Gate{timeout: time.Second, lookup: func(_ context.Context, account string, _ time.Time) (bool, error) {
		lookups.Add(1)
		return account[len(account)-1]%2 == 0, nil // even last byte: exhausted
	}}
	accounts := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		accounts = append(accounts, fmt.Sprintf("account_%03d", i))
	}
	first := gate.DeniedAmong(context.Background(), accounts, time.Now(), 10)
	if lookups.Load() != 10 {
		t.Fatalf("first round made %d lookups, want 10", lookups.Load())
	}
	for _, a := range first {
		if a[len(a)-1]%2 != 0 || !sort.StringsAreSorted(first) {
			t.Fatalf("round one listed %v", first)
		}
	}
	for round := 0; round < 12; round++ {
		gate.DeniedAmong(context.Background(), accounts, time.Now(), 10)
	}
	if lookups.Load() != 100 {
		t.Fatalf("after enough rounds %d lookups, want one per account (100)", lookups.Load())
	}
	all := gate.DeniedAmong(context.Background(), accounts, time.Now(), 10)
	if len(all) != 50 || lookups.Load() != 100 {
		t.Fatalf("fully cached round: %d denied, %d lookups; want 50 and no new lookups", len(all), lookups.Load())
	}
	var nilGate *Gate
	if nilGate.DeniedAmong(context.Background(), accounts, time.Now(), 10) != nil {
		t.Fatal("a nil gate listed accounts")
	}
}

// A lookup that fails after the account was answered "denied" must not reopen it
// (relay grants were re-issued over the cap within a minute when lookups timed out).
func TestGateKeepsARecentDenialWhenALookupFails(t *testing.T) {
	failing := atomic.Bool{}
	gate := &Gate{timeout: time.Second, lookup: func(context.Context, string, time.Time) (bool, error) {
		if failing.Load() {
			return false, errors.New("lookup timed out")
		}
		return true, nil
	}}
	ctx := context.Background()
	if !gate.Denied(ctx, "account_over", time.Now()) {
		t.Fatal("exhausted account was not withheld")
	}
	failing.Store(true)
	expire := func(age time.Duration) {
		gate.mu.Lock()
		e := gate.cache["account_over"]
		e.until = time.Now().Add(-time.Second)
		e.deniedAt = time.Now().Add(-age)
		gate.cache["account_over"] = e
		gate.mu.Unlock()
	}
	expire(time.Minute)
	if !gate.Denied(ctx, "account_over", time.Now()) {
		t.Fatal("a failed lookup reopened an account denied a minute ago")
	}
	expire(knownDeniedHold + time.Second)
	if gate.Denied(ctx, "account_over", time.Now()) {
		t.Fatal("a denial older than the hold still withheld grants after lookups failed")
	}
	if gate.Denied(ctx, "account_never_denied", time.Now()) {
		t.Fatal("an account never answered denied was withheld by a failing lookup")
	}
}
