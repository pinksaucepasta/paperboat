//go:build load

package app

// Simulated machine fleet for the per-machine background-cost budget (one PostgreSQL with 2-4 cores carries 3,000
// online machines). Two processes, so the server's CPU is not mixed with the load generator's:
//
//	TestFleetServe  the real router over the fleet database, on PAPERBOAT_FLEET_LISTEN, until killed.
//	TestFleetRun    seeds the fleet (accounts, machines with a control session each), then for each step of
//	                PAPERBOAT_FLEET_MACHINES runs every machine's idle loop for PAPERBOAT_FLEET_SECONDS and prints the
//	                request rate, the failures and the CPU cores the server process and PostgreSQL used.
//
// Each simulated machine does what an idle daemon does, signed with the real proof format: one held
// machine-events wait (answers 204 after 55 s; a 200 starts the next wait at once, as a daemon refetches), a
// runtime observation every PAPERBOAT_FLEET_OBSERVE_SECONDS (design: 240) and the signed-in client's inventory
// refresh (machines + workspaces + machine-services) every PAPERBOAT_FLEET_INVENTORY_SECONDS (design: 150) from the
// session of its account. tools/loadtest/run-fleet.sh starts an isolated PostgreSQL and drives both.
// Staging only: the DSN must name an isolated *_test database.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

const (
	fleetIssuer = "http://api.example.test"
	fleetKeyID  = "fleet"
)

// fleetSeed is the deterministic key material of the fleet: the server verifies machine credentials signed with it.
var fleetSeed = sha256.Sum256([]byte("paperboat fleet simulation"))

func fleetConfig(t *testing.T, dsn string) config.Config {
	cfg := config.Default()
	cfg.Database = config.Database{Driver: "postgres", DSN: dsn}
	cfg.HTTP.BrowserLoginOrigin = loadOrigin
	cfg.HTTP.PublicBaseURL, cfg.ReleaseBaseURL = fleetIssuer, "https://api.example.test"
	cfg.CLIAuth.VerificationURL, cfg.CLIAuth.MachinesURL = loadOrigin+"/cli", loadOrigin+"/machines"
	cfg.LocalAccessBaseDomain, cfg.RuntimeBaseDomain = "local.example.test", "runtime.example.test"
	cfg.Preview.BaseDomain, cfg.Tunnel.BaseDomain = "preview.example.test", "tunnel.example.test"
	cfg.Secrets.MintSigningKeys = []string{fleetKeyID + ":" + base64.RawURLEncoding.EncodeToString(fleetSeed[:])}
	cfg.CLIAuth.MintActiveKeyID = fleetKeyID
	return cfg
}

func fleetDSN(t *testing.T) string {
	dsn := os.Getenv("PAPERBOAT_PERF_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_PERF_DATABASE_DSN to an isolated *_test database")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func TestFleetServe(t *testing.T) {
	listen := os.Getenv("PAPERBOAT_FLEET_LISTEN")
	if listen == "" {
		t.Skip("set PAPERBOAT_FLEET_LISTEN")
	}
	cfg := fleetConfig(t, fleetDSN(t))
	cfg.HTTP.Address = listen
	store, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	application, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	// Run is the whole server: the HTTP listener plus the background workers and the LISTEN loops, the way
	// production runs it; serving only the handler left out the workers' cost and the event wake-ups.
	t.Logf("fleet server on %s", listen)
	if err := application.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type fleetMachine struct {
	id, env, account, token string
	cookie                  string
	key                     ed25519.PrivateKey
}

// fleetKey is the machine's own identity key (the server keeps public_identity_key unique per machine).
func fleetKey(i int) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("paperboat fleet machine " + strconv.Itoa(i)))
	return ed25519.NewKeyFromSeed(seed[:])
}

func fleetSession(account string) (string, string) {
	value := "fleet-session-" + account
	sum := sha256.Sum256([]byte(value))
	return value, hex.EncodeToString(sum[:])
}

// fleetSeedDatabase creates accounts and machines idempotently (a rerun with the same sizes changes nothing) and
// returns the machines with a signed machine-control credential each.
func fleetSeedDatabase(t *testing.T, store *db.DB, machines, accounts int) []fleetMachine {
	ctx := context.Background()
	signer, err := mint.ParseKeys([]string{fleetKeyID + ":" + base64.RawURLEncoding.EncodeToString(fleetSeed[:])}, fleetKeyID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id, workos_subject, primary_email, display_name, status, email_verified, canonical_email)
SELECT 'user_fleet_' || i, 'fleet_subject_' || i, 'fleet' || i || '@example.test', 'Fleet ' || i, 'active', true, 'fleet' || i || '@example.test'
FROM generate_series(1, $1::int) i ON CONFLICT DO NOTHING`, accounts)
	for account := 1; account <= accounts; account++ {
		id := "user_fleet_" + strconv.Itoa(account)
		_, hash := fleetSession(id)
		exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at,workos_session_id,workos_access_expires_at)
VALUES($1,$2,$3,$3,now()+interval '29 days','session_fleet',now()+interval '1 day')
ON CONFLICT (id) DO UPDATE SET session_hash=excluded.session_hash,expires_at=excluded.expires_at,revoked_at=NULL,workos_access_expires_at=excluded.workos_access_expires_at`,
			"session_"+id, id, hash)
	}
	now := time.Now().UTC()
	out := make([]fleetMachine, 0, machines)
	for i := 1; i <= machines; i++ {
		m := fleetMachine{id: "machine_fleet_" + strconv.Itoa(i), env: "env_fleet_" + strconv.Itoa(i), account: "user_fleet_" + strconv.Itoa(1+i%accounts), key: fleetKey(i)}
		public := m.key.Public().(ed25519.PublicKey)
		thumb := sha256.Sum256(public)
		exec(`INSERT INTO paperboat.user_machines(id, user_id, environment_id, platform, architecture, workspace_root, state, seat_state, online,
  enrolled_at, last_seen_at, alias, installation_generation, worker_generation, connector_state, configured_capabilities,
  observed_capabilities, host_service_version, public_identity_key)
VALUES($1, $2, $3, 'linux', 'amd64', '/home/fleet', 'online', 'occupied', true, now(), now(), $4, 1, 1, 'ready',
  ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host'], ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host'],
  '2026.11.19.1', $5) ON CONFLICT DO NOTHING`, m.id, m.account, m.env, "fleet"+strconv.Itoa(i), base64.RawURLEncoding.EncodeToString(public))
		m.cookie, _ = fleetSession(m.account)
		jti, operation := "mcc_fleet_"+strconv.Itoa(i), "mc_fleet_"+strconv.Itoa(i)
		m.token, err = signer.SignCredential(mint.CredentialInput{Issuer: config.NormalizeIssuer(fleetIssuer), Audience: "paperboat-control", Subject: m.id,
			JTI: jti, IssuedAt: now, ExpiresAt: now.Add(time.Hour), CredentialClass: "machine_control", Scopes: []string{"machine:connect", "machine:renew"},
			EnvironmentID: m.env, MachineID: m.id, UserID: m.account, KeyThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(thumb[:]),
			InstallationGeneration: 1, SessionGeneration: 1})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
		exec(`INSERT INTO paperboat.machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation)
VALUES($1,$2,1,$3,$4,$5,1) ON CONFLICT DO NOTHING`, operation, m.id, jti, now, now.Add(time.Hour))
		exec(`INSERT INTO paperboat.machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at)
VALUES($1,1,1,$2,$3,$4,$5) ON CONFLICT (machine_id) DO UPDATE SET operation_id=excluded.operation_id, credential_jti=excluded.credential_jti, issued_at=excluded.issued_at, expires_at=excluded.expires_at`,
			m.id, operation, jti, now, now.Add(time.Hour))
	}
	return out
}

// proof is the signed request proof of cli/internal/hostruntime/identity.Store.MachineProof.
func fleetProof(m fleetMachine, method, path string, body []byte) (string, string) {
	private := m.key
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	suffix, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	operation := "op-" + m.id + "-" + suffix.String()
	digest := sha256.Sum256(body)
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{"machine_id": m.id, "environment_id": m.env, "installation_generation": 1, "operation_id": operation,
		"method": method, "path": path, "body_sha256": base64.RawURLEncoding.EncodeToString(digest[:]), "issued_at": now, "expires_at": now.Add(time.Minute),
		"nonce": base64.RawURLEncoding.EncodeToString(nonce)})
	envelope, _ := json.Marshal(map[string]string{"alg": "EdDSA", "payload": base64.RawURLEncoding.EncodeToString(payload),
		"signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
	return base64.RawURLEncoding.EncodeToString(envelope), operation
}

// fleetEdges registers managed edge nodes (the real worker sweeps every workspace against them) and keeps their
// heartbeat fresh until ctx ends; without edges the edge-set sweep has nothing to place and costs nothing.
func fleetEdges(ctx context.Context, t *testing.T, store *db.DB, count int) {
	t.Helper()
	beat := func() {
		for i := 1; i <= count; i++ {
			_, err := store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at)
VALUES($1,'default','1.0','epoch','ready',true,now(),'region',$2,ARRAY['edge'],ARRAY['http3','http2'],100000,0,now(),now()+interval '1 hour','edge.example.test',27443,27444,'sha256:' || repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())
ON CONFLICT (id) DO UPDATE SET last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',capacity_observed_at=now(),ready=true,state='ready'`,
				"fleet_edge_"+strconv.Itoa(i), "domain-"+strconv.Itoa(i))
			if err != nil {
				t.Error(err)
				return
			}
		}
	}
	beat()
	go func() {
		for ticker := time.NewTicker(5 * time.Second); ; {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				beat()
			}
		}
	}()
}

type fleetStats struct {
	mu       sync.Mutex
	requests map[string]int
	failed   map[string]int
	wait200  atomic.Int64
	wait204  atomic.Int64
}

func (s *fleetStats) record(path string, status int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || status >= 400 {
		s.failed[path]++
	}
	s.requests[path]++
}

func fleetDo(ctx context.Context, client *http.Client, base string, m fleetMachine, method, path string, body []byte, stats *fleetStats, signed bool, extra func(*http.Request)) (int, []byte) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		stats.record(path, 0, err)
		return 0, nil
	}
	if signed {
		proof, operation := fleetProof(m, method, path, body)
		request.Header.Set("X-Paperboat-Machine-Proof", proof)
		request.Header.Set("Idempotency-Key", operation)
		request.Header.Set("X-Paperboat-Machine-Identity", m.token)
		request.Header.Set("Authorization", "Bearer "+m.token)
		request.Header.Set("Content-Type", "application/json")
	} else {
		request.AddCookie(&http.Cookie{Name: auth.DevSessionCookieName, Value: m.cookie})
	}
	if extra != nil {
		extra(request)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			stats.record(path, 0, err)
		}
		return 0, nil
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	stats.record(path, response.StatusCode, nil)
	return response.StatusCode, raw
}

func fleetLoop(ctx context.Context, base string, m fleetMachine, client *http.Client, stats *fleetStats, observe, inventory time.Duration, wg *sync.WaitGroup) {
	jitter := func(d time.Duration) time.Duration {
		return time.Duration(float64(d) * (0.5 + float64(time.Now().UnixNano()%1000)/1000))
	}
	topics := map[string]int64{}
	for _, name := range []string{"machine_access", "peer_network", "previews", "tunnels", "config", "inventory", "private_routes", "machine_services"} {
		topics[name] = 0
	}
	wg.Add(3)
	go func() { // held machine-events wait
		defer wg.Done()
		for ctx.Err() == nil {
			body, _ := json.Marshal(map[string]any{"topics": topics})
			status, raw := fleetDo(ctx, client, base, m, http.MethodPost, "/v1/machine-events/wait", body, stats, true, nil)
			switch status {
			case http.StatusOK:
				stats.wait200.Add(1)
				var answer struct {
					Topics map[string]int64 `json:"topics"`
				}
				if json.Unmarshal(raw, &answer) == nil {
					for name, generation := range answer.Topics {
						topics[name] = generation
					}
				}
			case http.StatusNoContent:
				stats.wait204.Add(1)
			default:
				time.Sleep(time.Second)
			}
		}
	}()
	go func() { // runtime observation heartbeat
		defer wg.Done()
		for sleep := jitter(observe); ; sleep = observe {
			select {
			case <-ctx.Done():
				return
			case <-time.After(sleep):
			}
			now := time.Now().UTC()
			body, _ := json.Marshal(map[string]any{"environment_id": m.env, "resource_id": m.id, "reporter_version": "fleet-sim", "sampled_at": now,
				"runtime_diagnostics": map[string]any{"capabilities": []string{"file_receive", "preview_launch", "terminal_host", "session_host", "ssh_host"},
					"worker_generation": 1, "os_boot_id": "boot-" + m.id, "worker_service_scope": "user", "connector_state": "ready", "connector_generation": 1, "observed_at": now}})
			fleetDo(ctx, client, base, m, http.MethodPost, "/v1/runtime-observations", body, stats, true, nil)
		}
	}()
	go func() { // the signed-in client's inventory refresh
		defer wg.Done()
		for sleep := jitter(inventory); ; sleep = inventory {
			select {
			case <-ctx.Done():
				return
			case <-time.After(sleep):
			}
			for _, path := range []string{"/v1/machines?limit=200&offset=0&sort=alias", "/v1/workspaces", "/v1/machine-services"} {
				fleetDo(ctx, client, base, m, http.MethodGet, path, nil, stats, false, nil)
			}
		}
	}()
}

func cgroupCores(t *testing.T, container string) float64 {
	out, err := exec.Command("docker", "exec", container, "cat", "/sys/fs/cgroup/cpu.stat").Output()
	if err != nil {
		t.Fatalf("read %s cpu.stat: %v", container, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if value, ok := strings.CutPrefix(line, "usage_usec "); ok {
			usec, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return usec / 1e6
		}
	}
	t.Fatalf("no usage_usec in %s cpu.stat", container)
	return 0
}

func processSeconds(t *testing.T, pid string) float64 {
	raw, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(raw[strings.LastIndex(string(raw), ")")+2:]))
	user, _ := strconv.ParseFloat(fields[11], 64)
	system, _ := strconv.ParseFloat(fields[12], 64)
	return (user + system) / 100 // clock ticks (CLK_TCK is 100 on Linux)
}

func envInts(name string, fallback []int) []int { return loadSteps(name, fallback) }

func TestFleetRun(t *testing.T) {
	base := strings.TrimSuffix(os.Getenv("PAPERBOAT_FLEET_URL"), "/")
	pid, container := os.Getenv("PAPERBOAT_FLEET_SERVER_PID"), os.Getenv("PAPERBOAT_FLEET_PG_CONTAINER")
	if base == "" || pid == "" || container == "" {
		t.Skip("set PAPERBOAT_FLEET_URL, PAPERBOAT_FLEET_SERVER_PID and PAPERBOAT_FLEET_PG_CONTAINER (tools/loadtest/run-fleet.sh)")
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: fleetDSN(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounts := envInts("PAPERBOAT_FLEET_ACCOUNTS", []int{300})[0]
	seconds := time.Duration(envInts("PAPERBOAT_FLEET_SECONDS", []int{300})[0]) * time.Second
	settle := time.Duration(envInts("PAPERBOAT_FLEET_SETTLE_SECONDS", []int{90})[0]) * time.Second
	observe := time.Duration(envInts("PAPERBOAT_FLEET_OBSERVE_SECONDS", []int{240})[0]) * time.Second
	inventory := time.Duration(envInts("PAPERBOAT_FLEET_INVENTORY_SECONDS", []int{150})[0]) * time.Second
	for _, machines := range envInts("PAPERBOAT_FLEET_MACHINES", []int{500, 1500, 3000}) {
		fleet := fleetSeedDatabase(t, store, machines, accounts)
		edgeCtx, stopEdges := context.WithCancel(context.Background())
		fleetEdges(edgeCtx, t, store, 3)
		client := &http.Client{Transport: &http.Transport{MaxIdleConns: machines + 64, MaxIdleConnsPerHost: machines + 64, MaxConnsPerHost: machines + 64}}
		// Behind the production proxy configuration (tools/loadtest/run-fleet.sh with PAPERBOAT_FLEET_PROXY=1) every machine
		// has its own TLS connection, as a daemon in its own process does; the proxy's local CA is not in the trust store.
		proxy := os.Getenv("PAPERBOAT_FLEET_PROXY_CONTAINER")
		clientFor := func() *http.Client {
			if proxy == "" {
				return client
			}
			return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true}}
		}
		stats := &fleetStats{requests: map[string]int{}, failed: map[string]int{}}
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i, m := range fleet {
			fleetLoop(ctx, base, m, clientFor(), stats, observe, inventory, &wg)
			if i%50 == 49 {
				time.Sleep(200 * time.Millisecond) // ramp: a connection storm is not the steady state under test
			}
		}
		time.Sleep(settle)
		pg0, srv0, at := cgroupCores(t, container), processSeconds(t, pid), time.Now()
		var proxy0 float64
		if proxy != "" {
			proxy0 = cgroupCores(t, proxy)
		}
		stats.mu.Lock()
		before := map[string]int{}
		total0 := 0
		for k, v := range stats.requests {
			before[k], total0 = v, total0+v
		}
		stats.mu.Unlock()
		w200, w204 := stats.wait200.Load(), stats.wait204.Load()
		time.Sleep(seconds)
		elapsed := time.Since(at).Seconds()
		pg, srv := (cgroupCores(t, container)-pg0)/elapsed, (processSeconds(t, pid)-srv0)/elapsed
		if proxy != "" {
			cores := (cgroupCores(t, proxy) - proxy0) / elapsed
			t.Logf("proxy (Caddy, production config) %.3f cores (%.2f millicores/machine) for the same window", cores, cores*1000/float64(machines))
		}
		stats.mu.Lock()
		total1, failed := 0, 0
		var lines []string
		for k, v := range stats.requests {
			total1 += v
			lines = append(lines, fmt.Sprintf("%-45s %7.1f req/min (%.2f per machine per min)", k, float64(v-before[k])*60/elapsed, float64(v-before[k])*60/elapsed/float64(machines)))
		}
		for _, v := range stats.failed {
			failed += v
		}
		stats.mu.Unlock()
		slices.Sort(lines)
		t.Logf("machines=%d accounts=%d window=%.0fs: %.0f req/s (%.2f per machine per min), failed %d (total), waits 200=%d 204=%d\n  postgres %.3f cores (%.2f millicores/machine), server %.3f cores (%.2f millicores/machine)\n  %s",
			machines, accounts, elapsed, float64(total1-total0)/elapsed, float64(total1-total0)*60/elapsed/float64(machines), failed, stats.wait200.Load()-w200, stats.wait204.Load()-w204,
			pg, pg*1000/float64(machines), srv, srv*1000/float64(machines), strings.Join(lines, "\n  "))
		cancel()
		stopEdges()
		wg.Wait()
		client.CloseIdleConnections()
	}
}
