//go:build load

package controlplane

// F21-170: config sync under load, no storm and no slow neighbor. Machines of many teams report their sync status
// (Record: a machine proof, the assignment re-read, and, for a team-adopted assignment that has published, the global
// team write lock and an audit row) while one unrelated machine reports and its owner reads the status list; the
// neighbor's latency under that load against its latency alone is the slow-neighbor check, the database's
// idle-in-transaction and lock-wait samples are the storm check, and the status list of a 200-machine account must
// page from the server's total (every machine once, no page over 200 rows) while the load runs.
//
//	PAPERBOAT_TEST_DATABASE_DSN=... go test -tags load -run TestLoadConfigStatus -v ./internal/controlplane/
//	(PAPERBOAT_LOAD_CFG_TEAMS, PAPERBOAT_LOAD_CFG_SECONDS: 20 teams of 5 machines and 20 s by default)

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

type loadConfigMachine struct {
	env, team, member string
	identity          HelperIdentity
	key               ed25519.PrivateKey
	report            ConfigStatusReport
	revision          int64
}

type loadConfigFixture struct {
	t        *testing.T
	ctx      context.Context
	statuses *ConfigStatusService
	machines []*loadConfigMachine
}

// loadConfigTeam makes one team whose member owns `count` machines, all adopting the team's default.
func (f *loadConfigFixture) team(enrollment *EnrollmentService, assignments *ConfigAssignmentService, tag string, count int) []*loadConfigMachine {
	t, ctx := f.t, f.ctx
	owner, member, team := "loadcfg_"+tag+"_o", "loadcfg_"+tag+"_m", "loadcfg_"+tag+"_t"
	envs, machines := make([]string, count), make([]string, count)
	for i := range count {
		envs[i], machines[i] = fmt.Sprintf("loadcfg_%s_e%03d", tag, i), fmt.Sprintf("loadcfg_%s_u%03d", tag, i)
	}
	cleanupConfigTeamLifecycleFixture(t, assignments.store, []string{owner, member}, []string{team}, envs, machines)
	store := assignments.store
	for _, user := range []string{owner, member} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_"+user, user+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.teams (team_id,owner_account,generation) VALUES ($1,$2,1)`, []any{team, owner}},
		{`INSERT INTO paperboat.team_members (team_id,account_id,membership_generation,role,active) VALUES ($1,$2,1,'member',true)`, []any{team, member}},
	} {
		if _, err := store.SQL().ExecContext(ctx, s.query, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	for i := range count {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$2,$3)`, envs[i], "workspace_"+tag, member); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,seat_state) VALUES ($1,$2,$3,'linux','unknown','/workspace','occupied')`, machines[i], member, envs[i]); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := assignments.ConnectRepository(ctx, owner, "github", "loadcfg-"+tag, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assignments.ConnectRepository(ctx, member, "github", "loadcfg-"+tag, ""); err != nil { // adopting needs the member's own connection to it
		t.Fatal(err)
	}
	if _, err := assignments.SetTeamDefault(ctx, owner, team, repo.ID, 0); err != nil {
		t.Fatal(err)
	}
	// Enrol before adopting, adoption assigns every live machine of the member at once.
	out := make([]*loadConfigMachine, count)
	for i := range count {
		grant, err := enrollment.Issue(ctx, member, fmt.Sprintf("loadcfg-%s-%03d", tag, i), envs[i], 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		key := enrollmentTestPrivateKey(envs[i])
		identity, err := enrollment.Exchange(ctx, grant.Credential, key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = &loadConfigMachine{env: envs[i], team: team, member: member, identity: identity, key: key}
	}
	if _, err := assignments.AdoptTeamDefault(ctx, member, team, 1); err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		assignment, err := store.Queries().GetControlConfigAssignment(ctx, m.env)
		if err != nil {
			t.Fatal(err)
		}
		machine, err := store.Queries().GetCanonicalMachineForEnvironment(ctx, m.env)
		if err != nil {
			t.Fatal(err)
		}
		m.report = ConfigStatusReport{State: "healthy", Mode: assignment.Mode, RepositoryID: assignment.RepositoryID.String, AssignmentID: assignment.ID,
			EnvironmentID: m.env, MachineID: m.identity.MachineID, InstallationGeneration: machine.InstallationGeneration, WarningRevision: assignment.WarningRevision.String,
			PolicyRevision: "policy-1", LastPublishedRevision: "published-0"}
	}
	f.machines = append(f.machines, out...)
	return out
}

// send is one status report with a fresh proof; publish makes it a new published revision.
func (f *loadConfigFixture) send(m *loadConfigMachine, publish bool) error {
	m.revision++
	m.report.SyncRevision = m.revision
	now := time.Now().UTC()
	m.report.UpdatedAt = now
	if publish {
		m.report.LastPublishedRevision = fmt.Sprintf("published-%d", m.revision)
	}
	body := []byte(`{}`)
	digest := sha256.Sum256(body)
	claims := HelperProofClaims{HelperID: m.identity.HelperID, EnvironmentID: m.env, OperationID: fmt.Sprintf("op-%s-%d", m.env, m.revision), Nonce: proofTestNonce(),
		Method: "POST", Path: "/v1/config/status", BodySHA256: base64.RawURLEncoding.EncodeToString(digest[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	payload, _ := json.Marshal(claims)
	proof, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(m.key, payload))})
	return f.statuses.Record(f.ctx, m.identity.Credential, proof, body, "POST", "/v1/config/status", m.report)
}

func loadP99(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	return values[min(len(values)-1, len(values)*99/100)]
}

// pageAll reads a member's status list in pages of `limit` from the server's keyset cursor and returns the total the server reported.
func (f *loadConfigFixture) pageAll(m *loadConfigMachine, limit int) (seen, total int, err error) {
	ctx := workspace.WithContext(f.ctx, workspace.Scope{ID: m.team, TeamID: m.team})
	var after *ConfigStatusPosition
	ids := map[string]bool{}
	for {
		page, err := f.statuses.AccountInventory(ctx, m.member, ConfigStatusInventoryOptions{Limit: limit, After: after})
		if err != nil {
			return 0, 0, err
		}
		if len(page.Environments) > limit {
			return 0, 0, fmt.Errorf("a page of %d rows for limit %d", len(page.Environments), limit)
		}
		for _, e := range page.Environments {
			if ids[e.EnvironmentID] {
				return 0, 0, fmt.Errorf("environment %s listed twice", e.EnvironmentID)
			}
			ids[e.EnvironmentID] = true
		}
		total, _ = page.Pagination["total"].(int)
		if page.Next == nil {
			return len(ids), total, nil
		}
		after = page.Next
	}
}

func TestLoadConfigStatus(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	teamsN, seconds := 20, 20
	if v, err := strconv.Atoi(os.Getenv("PAPERBOAT_LOAD_CFG_TEAMS")); err == nil && v > 0 {
		teamsN = v
	}
	if v, err := strconv.Atoi(os.Getenv("PAPERBOAT_LOAD_CFG_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	signer, err := mint.New([]mint.Key{{ID: "config-load", PrivateKey: ed25519.NewKeyFromSeed([]byte("config-load-seed-0123456789abcde"))}}, "config-load", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := NewEnrollmentService(store, signer, audit.NewWriter(store), "https://api.example.test", "config-load-encryption-key")
	assignments := NewConfigAssignmentService(store, audit.NewWriter(store), "warning-load")
	assignments.SetRepositoryResolver(testConfigRepositoryResolver())
	f := &loadConfigFixture{t: t, ctx: ctx, statuses: NewConfigStatusService(store, enrollment, 10)}
	neighbor := f.team(enrollment, assignments, run+"n", 1)[0]
	big := f.team(enrollment, assignments, run+"b", 200)
	var groups [][]*loadConfigMachine
	for i := range teamsN + 4 { // the last 4 teams report back to back
		groups = append(groups, f.team(enrollment, assignments, fmt.Sprintf("%sw%03d", run, i), 5))
	}

	// The neighbor reports its status and reads its status list every 10 ms; it reports its own latencies.
	probe := func(until time.Time) (writes, reads []time.Duration, failures int) {
		scope := workspace.WithContext(ctx, workspace.Scope{ID: neighbor.team, TeamID: neighbor.team})
		for time.Now().Before(until) {
			start := time.Now()
			if err := f.send(neighbor, false); err != nil {
				failures++
			}
			writes = append(writes, time.Since(start))
			start = time.Now()
			if _, err := f.statuses.AccountInventory(scope, neighbor.member, ConfigStatusInventoryOptions{Limit: 50}); err != nil {
				failures++
			}
			reads = append(reads, time.Since(start))
			time.Sleep(10 * time.Millisecond)
		}
		return
	}
	baseWrites, baseReads, baseFailures := probe(time.Now().Add(5 * time.Second))
	if baseFailures != 0 {
		t.Fatalf("the neighbor alone failed %d times", baseFailures)
	}
	t.Logf("neighbor alone: report p99 %v, status list p99 %v", loadP99(baseWrites), loadP99(baseReads))

	// phase: every machine of `teams` reports every `pause` (every 10th report publishes a new revision, the rest repeat the
	// last published one, which still takes the team lock), 4 readers page the 200-machine account, the neighbor probes, and
	// the database is sampled for idle-in-transaction sessions and lock waits.
	phase := func(name string, teams [][]*loadConfigMachine, pause time.Duration, assertNeighbor bool) {
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
		var reports, pages, errorsTotal atomic.Int64
		var firstError atomic.Value
		fail := func(err error) {
			errorsTotal.Add(1)
			firstError.CompareAndSwap(nil, err.Error())
		}
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		var load sync.WaitGroup
		machinesN := 0
		for _, team := range teams {
			for _, m := range team {
				machinesN++
				load.Add(1)
				go func() {
					defer load.Done()
					for seq := 1; time.Now().Before(deadline); seq++ {
						if err := f.send(m, seq%10 == 0); err != nil {
							fail(err)
						} else {
							reports.Add(1)
						}
						time.Sleep(pause)
					}
				}()
			}
		}
		for r := range 4 {
			load.Add(1)
			go func() {
				defer load.Done()
				for time.Now().Before(deadline) {
					seen, total, err := f.pageAll(big[r], 50)
					switch {
					case err != nil:
						fail(err)
					case seen != len(big) || total != len(big):
						fail(fmt.Errorf("status list paged %d machines with server total %d, want %d", seen, total, len(big)))
					default:
						pages.Add(1)
					}
					time.Sleep(time.Second)
				}
			}()
		}
		loadWrites, loadReads, loadFailures := probe(deadline)
		load.Wait()
		close(stop)
		sampling.Wait()
		t.Logf("%s: %d machines in %d teams, a report every %v for %d s: %d reports (%.1f/s), %d full status-list pages of 200 machines, %d worker errors (first: %v)", name, machinesN, len(teams), pause, seconds, reports.Load(), float64(reports.Load())/float64(seconds), pages.Load(), errorsTotal.Load(), firstError.Load())
		t.Logf("%s: neighbor report p99 %v (%d samples), status list p99 %v (%d samples), %d failures; database: max idle-in-transaction %d, max sessions waiting on a lock %d", name, loadP99(loadWrites), len(loadWrites), loadP99(loadReads), len(loadReads), loadFailures, maxIdleInTx.Load(), maxLockWait.Load())
		if errorsTotal.Load() != 0 || loadFailures != 0 {
			t.Errorf("%s: failures under load: workers %d (%v), neighbor %d", name, errorsTotal.Load(), firstError.Load(), loadFailures)
		}
		if int(maxIdleInTx.Load()) > machinesN+8 {
			t.Errorf("%s: idle-in-transaction sessions: %d for %d machines", name, maxIdleInTx.Load(), machinesN)
		}
		if assertNeighbor {
			if got, bound := loadP99(loadWrites), max(20*loadP99(baseWrites), time.Second); got > bound {
				t.Errorf("%s: slow neighbor: report p99 %v under load against %v alone (bound %v)", name, got, loadP99(baseWrites), bound)
			}
			if got, bound := loadP99(loadReads), max(20*loadP99(baseReads), time.Second); got > bound {
				t.Errorf("%s: slow neighbor: status list p99 %v under load against %v alone (bound %v)", name, got, loadP99(baseReads), bound)
			}
		}
	}
	phase("daily", groups[:teamsN], time.Second, true)
	phase("saturating", groups[teamsN:], 0, false)

	// The page size is bounded while the database is busy: a request over 200 rows is refused, not served.
	scope := workspace.WithContext(ctx, workspace.Scope{ID: big[0].team, TeamID: big[0].team})
	if _, err := f.statuses.AccountInventory(scope, big[0].member, ConfigStatusInventoryOptions{Limit: 201}); !errors.Is(err, ErrConfigStatusInvalid) {
		t.Errorf("a status list limit of 201: %v, want ErrConfigStatusInvalid", err)
	}
}
