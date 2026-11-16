//go:build load

package peersessions

// TestLoadPeerPolicy measures the peer configuration path's capacity: N simulated idle endpoints renewing at the idle
// profile, then M active users running exec cycles on top, against PostgreSQL on another host (as in production). It
// prints server CPU and database CPU per request and the idle machines and active users one core carries. It builds
// against main as well, so the same file measures before and after the in-memory policy documents.
//
// Idle profile (cli/internal/hostruntime/runtime/production_carrier.go carrierIdleRenewal, peertransport/tailnet
// authority.go IdleRefresh): a machine renews about once a minute, a CLI endpoint every 20 minutes. An active user's
// exec: admitted by the host's standing rule with no control-plane request; the host uploads its queued audit events
// every 30 s in one request (FIX-EXECDB). Before, each exec created an access session, both endpoints fetched their
// configuration, the session was revoked and both fetched again (four configurations and two writes).
//
// Never against production: PAPERBOAT_PERF_DATABASE_DSN must name an isolated *_test database.
//
//	PEER_LOAD_DBCPU=1 \
//	PAPERBOAT_PERF_DATABASE_DSN=... ./peersessions.load.test -test.run TestLoadPeerPolicy -test.v -test.timeout 60m

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
)

func loadEnv(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}

func processCPU() time.Duration {
	var u syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

var usageUsec = regexp.MustCompile(`usage_usec (\d+)`)

// databaseCPU is the database container's CPU time (its cgroup's usage_usec, read through pg_read_file as the
// superuser the isolated test database runs as), or -1 with PEER_LOAD_DBCPU unset.
func databaseCPU(t *testing.T, store *db.DB) time.Duration {
	if os.Getenv("PEER_LOAD_DBCPU") == "" {
		return -1
	}
	var stat string
	if err := store.SQL().QueryRow(`SELECT pg_read_file('/sys/fs/cgroup/cpu.stat')`).Scan(&stat); err != nil {
		t.Fatalf("database CPU: %v", err)
	}
	m := usageUsec.FindStringSubmatch(stat)
	if m == nil {
		t.Fatalf("database CPU: no usage_usec in %q", stat)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return time.Duration(n) * time.Microsecond
}

type loadEndpoint struct {
	account, id string
	machine     bool
}

func TestLoadPeerPolicy(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_PERF_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_PERF_DATABASE_DSN to an isolated *_test database")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	accounts, perAccount := loadEnv("PEER_LOAD_ACCOUNTS", 100), loadEnv("PEER_LOAD_MACHINES_PER_ACCOUNT", 9)
	sharedHosts, active := loadEnv("PEER_LOAD_TEAM_HOSTS", 5), loadEnv("PEER_LOAD_ACTIVE", 20)
	window := time.Duration(loadEnv("PEER_LOAD_WINDOW_SECONDS", 120)) * time.Second
	machineEvery := time.Duration(loadEnv("PEER_LOAD_MACHINE_RENEW_SECONDS", 60)) * time.Second
	cliEvery := time.Duration(loadEnv("PEER_LOAD_CLI_RENEW_SECONDS", 1200)) * time.Second
	activeEvery := time.Duration(loadEnv("PEER_LOAD_ACTIVE_CYCLE_SECONDS", 10)) * time.Second

	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	_, signer, _ := ed25519.GenerateKey(rand.Reader)
	provider, err := mint.New([]mint.Key{{ID: "peer-load", PrivateKey: signer}}, "peer-load", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureQuota("https://console.example.test/billing")
	// The in-memory documents keep their own LISTEN session when the build has them (not on main).
	type policyListener interface {
		ListenPolicy(context.Context, policyListenPool, time.Duration) error
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	if l, ok := any(service).(policyListener); ok {
		t.Log("policy cache: on")
		go func() {
			for runCtx.Err() == nil {
				if l.ListenPolicy(runCtx, store.Direct(), 24*time.Second) != nil {
					time.Sleep(time.Second)
				}
			}
		}()
	} else {
		t.Log("policy cache: not in this build")
	}

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	run := fmt.Sprint(time.Now().Unix())
	now := time.Now().UTC().Truncate(time.Second)
	var endpoints []loadEndpoint
	var clis []loadEndpoint
	firstMachine := map[string]string{}
	seedStart := time.Now()
	for a := range accounts {
		account := fmt.Sprintf("load_%s_%04d", run, a)
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
		for m := range perAccount {
			machine := fmt.Sprintf("%s_m%02d", account, m)
			exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, "env_"+machine, account)
			exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, account, "env_"+machine)
			endpoints = append(endpoints, loadEndpoint{account, machine, true})
			if m == 0 {
				firstMachine[account] = machine
			}
		}
		exec(`SELECT paperboat.ensure_machine_address_leases($1, ARRAY(SELECT id FROM paperboat.user_machines WHERE user_id=$1))`, account)
		cli := account + "_cli"
		exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'load','desktop','test',ARRAY['machines:connect'],'active',$3,$3)`, cli, account, now)
		endpoints = append(endpoints, loadEndpoint{account, cli, false})
		clis = append(clis, loadEndpoint{account, cli, false})
	}
	for _, e := range endpoints {
		role, wireRole := "cli", wire.RoleCLI
		if e.machine {
			role, wireRole = "machine", wire.RoleMachine
		}
		_, _, quic := networkEndpointTLS(t, e.id, now)
		certificate, fingerprint := networkCertificateWithKeys(t, signer, e.account, wireRole, e.id, 1, 1, quic, now, now.Add(6*time.Hour))
		exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,$4,1,1,$5,$6,$7,$8,'peer-load',$9)`,
			fingerprint[:], e.account, e.id, role, certificate, quic[:], now, now.Add(6*time.Hour), signer.Public().(ed25519.PublicKey))
		private, public := networkX25519Key(t)
		registration := NetworkRegistration{OperationID: "register_" + e.id, UserID: e.account, EndpointID: e.id, Role: role, EndpointGeneration: 1, WireGuardPublicKey: public, DiscoPublicKey: networkDiscoPublic(t, private), QUICCertificateFingerprint: fingerprint[:]}
		if e.machine {
			registration.MachineID, registration.MachineGeneration = e.id, 1
		}
		if _, err := service.Register(ctx, registration); err != nil {
			t.Fatalf("register %s: %v", e.id, err)
		}
	}
	// One team: every account is a member; the first account shares its first sharedHosts machines with all members.
	team := "load_team_" + run
	owner := clis[0].account
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 day')`, "billing_"+team, team)
	for i, c := range clis {
		role := "member"
		if i == 0 {
			role = "admin"
		}
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,$3,true)`, team, c.account, role)
	}
	for h := range min(sharedHosts, perAccount) {
		host := fmt.Sprintf("%s_m%02d", owner, h)
		exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, host, owner)
		teamstest.SetMachineAccess(t, store, team, host, "", "terminal", "exec")
	}
	relay := "load_relay_" + run
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at)
 VALUES($1,'load','v1','epoch','relay.example.test',443,444,'ready',true,now(),1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100000,1,now(),now()+interval '1 day')`, relay)
	go func() { // the relay's heartbeat, as a live node writes it
		for runCtx.Err() == nil {
			_, _ = store.SQL().ExecContext(runCtx, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now(),capacity_observed_at=now() WHERE id=$1`, relay)
			select {
			case <-runCtx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}()
	t.Logf("seeded %d accounts x (%d machines + 1 CLI) = %d endpoints, team of %d with %d shared hosts in %v", accounts, perAccount, len(endpoints), len(clis), sharedHosts, time.Since(seedStart).Round(time.Second))

	var requests, failures atomic.Int64
	var mu sync.Mutex
	var latencies []time.Duration
	configure := func(e loadEndpoint, live []AdmittedScope) {
		started := time.Now()
		_, err := service.Configuration(runCtx, NetworkConfigRequest{OperationID: fmt.Sprintf("load_%s_%d", e.id, started.UnixNano()), UserID: e.account, EndpointID: e.id, Authority: AuthorityUntilRevoked, AdmittedScopes: live})
		if runCtx.Err() != nil {
			return
		}
		requests.Add(1)
		if err != nil {
			if failures.Add(1) <= 5 {
				t.Logf("configuration %s: %v", e.id, err)
			}
			return
		}
		mu.Lock()
		latencies = append(latencies, time.Since(started))
		mu.Unlock()
	}
	// Warm: every endpoint once (a fresh server's first renewals).
	warm := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, e := range endpoints {
		wg.Add(1)
		sem <- struct{}{}
		go func() { defer wg.Done(); configure(e, nil); <-sem }()
	}
	wg.Wait()
	t.Logf("warm-up: %d configurations in %v, %d failed", requests.Load(), time.Since(warm).Round(time.Millisecond), failures.Load())

	// The idle fleet runs for the rest of the test, each endpoint at its renewal interval, spread evenly.
	for i, e := range endpoints {
		every := cliEvery
		if e.machine {
			every = machineEvery
		}
		go func() {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(time.Duration(i) * every / time.Duration(len(endpoints))):
			}
			for runCtx.Err() == nil {
				next := time.Now().Add(every)
				configure(e, nil)
				select {
				case <-runCtx.Done():
				case <-time.After(time.Until(next)):
				}
			}
		}()
	}
	machines := 0
	for _, e := range endpoints {
		if e.machine {
			machines++
		}
	}
	idleRate := float64(machines)/machineEvery.Seconds() + float64(len(clis))/cliEvery.Seconds()
	measure := func(name string) (cpu, dbCPU time.Duration, n int64) {
		time.Sleep(machineEvery / 4) // let the phase settle
		requests.Store(0)
		failures.Store(0)
		mu.Lock()
		latencies = latencies[:0]
		mu.Unlock()
		cpu0, db0, started := processCPU(), databaseCPU(t, store), time.Now()
		time.Sleep(window)
		cpu, dbCPU, elapsed := processCPU()-cpu0, databaseCPU(t, store)-db0, time.Since(started)
		if db0 < 0 {
			dbCPU = -1
		}
		n = requests.Load()
		mu.Lock()
		values := slices.Clone(latencies)
		mu.Unlock()
		slices.Sort(values)
		p50, p99 := time.Duration(0), time.Duration(0)
		if len(values) > 0 {
			p50, p99 = values[len(values)/2], values[len(values)*99/100]
		}
		out, _ := json.Marshal(map[string]any{"phase": name, "seconds": elapsed.Seconds(), "configurations": n, "failed": failures.Load(),
			"per_s": float64(n) / elapsed.Seconds(), "p50_ms": p50.Milliseconds(), "p99_ms": p99.Milliseconds(),
			"server_cpu_cores": cpu.Seconds() / elapsed.Seconds(), "db_cpu_cores": dbCPU.Seconds() / elapsed.Seconds(), "gomaxprocs": runtime.GOMAXPROCS(0)})
		t.Logf("RESULT %s", out)
		return cpu, dbCPU, n
	}
	idleCPU, idleDB, idleN := measure("idle")
	perReq := idleCPU.Seconds() / float64(max(idleN, 1))
	dbPerReq := idleDB.Seconds() / float64(max(idleN, 1))
	perMachine := 1/machineEvery.Seconds() + 1/cliEvery.Seconds()/float64(perAccount) // a machine plus its share of the account's CLI

	// Active users on top of the idle fleet: each exec on one of the user's own machines. The host admits it from its
	// standing rule (no access session, no configuration fetch), queues the start for audit and uploads its queue every
	// offlineaudit.UploadInterval (30 s) in one request, as cli/internal/hostruntime/offlineaudit does.
	audits := usermachines.New(store, audit.NewWriter(store), usermachines.Policy{}, nil)
	var cycles atomic.Int64
	for i := range min(active, len(clis)) {
		c := clis[i]
		host := loadEndpoint{c.account, firstMachine[c.account], true}
		go func() {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(time.Duration(i) * activeEvery / time.Duration(active)):
			}
			upload := time.Now().Add(30 * time.Second)
			var queued []usermachines.OfflineAuditEvent
			for n := 0; runCtx.Err() == nil; n++ {
				next := time.Now().Add(activeEvery)
				queued = append(queued, usermachines.OfflineAuditEvent{EventID: fmt.Sprintf("load_%s_%d", c.id, n), Kind: "offline_session_started", Capability: "exec", PeerEndpointID: c.id, PeerAccountID: c.account, OperationID: fmt.Sprintf("op_%s_%d", c.id, n), OccurredAt: time.Now().UTC().Truncate(time.Second), Origin: "online"})
				cycles.Add(1)
				if !time.Now().Before(upload) {
					if _, err := audits.RecordOfflineAuditBacklog(runCtx, host.id, 1, queued); err != nil && runCtx.Err() == nil {
						t.Logf("audit upload: %v", err)
					}
					queued, upload = queued[:0], time.Now().Add(30*time.Second)
				}
				select {
				case <-runCtx.Done():
				case <-time.After(time.Until(next)):
				}
			}
		}()
	}
	activeCPU, activeDB, _ := measure("idle+active")
	users := float64(min(active, len(clis)))
	perUser := (activeCPU - idleCPU).Seconds() / window.Seconds() / users
	dbPerUser := (activeDB - idleDB).Seconds() / window.Seconds() / users
	summary := map[string]any{"endpoints": len(endpoints), "machines": machines, "idle_config_per_s": idleRate,
		"server_ms_per_config": perReq * 1000, "db_ms_per_config": dbPerReq * 1000,
		"idle_machines_per_server_core": 1 / (perMachine * perReq), "active_users": users, "active_cycles": cycles.Load(),
		"server_core_per_active_user": perUser, "active_users_per_server_core": 1 / perUser}
	if idleDB >= 0 {
		summary["idle_machines_per_db_core"] = 1 / (perMachine * dbPerReq)
		summary["db_core_per_active_user"] = dbPerUser
		summary["active_users_per_db_core"] = 1 / dbPerUser
	}
	out, _ := json.Marshal(summary)
	t.Logf("SUMMARY %s", out)
}
