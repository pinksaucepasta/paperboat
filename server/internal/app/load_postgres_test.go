//go:build load

package app

// Server load checks for the scale matrix (PLAN 3b, Lane W). They boot the real router on a real listener over the
// seeded perf database and measure where the control plane first degrades:
//
//   - TestLoadHeldEventWaiters: N daemons holding the event long-poll with no changes. Reports database queries per
//     second (the cost of holding waiters) and checks every wait answers 204 inside the client's 30 s header timeout.
//   - TestLoadFailover: through a proxy in front of several instances, kills one mid-test and measures the failure
//     window (tools/loadtest/run-multi-instance.sh).
//   - TestLoadIdlePollMix: N simulated daemons each running the inventory-refresh request mix every 15 s, ramped
//     until p99 exceeds 5 s or 1% of requests fail. Reports the first N that degrades and whether it fails with
//     clear errors or 5xx.
//
// Never against production: PAPERBOAT_PERF_DATABASE_DSN must name an isolated *_test database (staging VM).
//
//	PAPERBOAT_PERF_DATABASE_DSN=... go test -tags load -run TestLoad -v -timeout 60m ./internal/app/

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
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
	"github.com/pinksaucepasta/paperboat/server/internal/perf"
)

const loadOrigin = "http://console.example.test"

type loadServer struct {
	url, session, csrf string
	machines           []string
	store              *db.DB
}

// transactions is the database's committed + rolled-back transaction count (pg_stat_database). It measures the
// load the same way whether the server under test runs in this process or as separate instances behind a proxy.
func (s *loadServer) transactions(t *testing.T) int64 {
	t.Helper()
	var count int64
	if err := s.store.SQL().QueryRowContext(context.Background(), `SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = current_database()`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// startLoadServer seeds the perf data set and a browser session for the solo actor. With PAPERBOAT_LOAD_TARGET_URL
// set it targets that URL (server instances started separately, e.g. several behind a proxy); otherwise it serves the
// real router in this process.
func startLoadServer(t *testing.T) *loadServer {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_PERF_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_PERF_DATABASE_DSN to an isolated *_test database")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database = config.Database{Driver: "postgres", DSN: dsn}
	cfg.HTTP.BrowserLoginOrigin = loadOrigin
	store, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := perf.Prepare(ctx, store); err != nil {
		t.Fatal(err)
	}
	server := &loadServer{session: "load-session-solo", csrf: "load-csrf-solo", store: store}
	hash := func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at,workos_session_id,workos_access_expires_at)
VALUES('session_load_solo',$1,$2,$3,now()+interval '29 days','session_load',now()+interval '1 day')
ON CONFLICT (id) DO UPDATE SET session_hash=excluded.session_hash,csrf_hash=excluded.csrf_hash,expires_at=excluded.expires_at,revoked_at=NULL,workos_access_expires_at=excluded.workos_access_expires_at`,
		perf.Solo, hash(server.session), hash(server.csrf)); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SQL().QueryContext(ctx, `SELECT id FROM paperboat.user_machines WHERE user_id=$1 AND owner_team_id IS NULL AND state='online' ORDER BY id`, perf.Solo)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		server.machines = append(server.machines, id)
	}
	rows.Close()
	if len(server.machines) == 0 {
		t.Fatal("perf seed has no online solo machines")
	}
	if target := strings.TrimSuffix(os.Getenv("PAPERBOAT_LOAD_TARGET_URL"), "/"); target != "" {
		server.url = target
		return server
	}
	application, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.db.Close() })
	listener := httptest.NewServer(application.server.Handler)
	t.Cleanup(listener.Close)
	server.url = listener.URL
	return server
}

func (s *loadServer) request(ctx context.Context, client *http.Client, method, path, body string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, method, s.url+path, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.AddCookie(&http.Cookie{Name: auth.DevSessionCookieName, Value: s.session})
	if method != http.MethodGet {
		request.AddCookie(&http.Cookie{Name: auth.DevCSRFCookieName, Value: s.csrf})
		request.Header.Set(auth.CSRFHeaderName, s.csrf)
		request.Header.Set("Origin", loadOrigin)
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode, nil
}

// The client side of a released daemon: 30 s response header timeout (cli/internal/httptransport DefaultConfig).
func loadClient(conns int) *http.Client {
	return &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second, MaxIdleConns: conns, MaxIdleConnsPerHost: conns, MaxConnsPerHost: conns}}
}

func TestLoadHeldEventWaiters(t *testing.T) {
	server := startLoadServer(t)
	for _, waiters := range loadSteps("PAPERBOAT_LOAD_WAITERS", []int{100, 500, 1000, 2000}) {
		client := loadClient(waiters)
		body := `{"topics":{"inventory":0,"machine_services":0,"peer_network":0}}`
		// Learn the current generations so the held waits see no change.
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		if status, err := server.request(ctx, client, http.MethodPost, "/v1/machines/"+server.machines[0]+"/events/wait", body); err != nil || status >= 500 {
			cancel()
			t.Fatalf("first wait: status %d err %v", status, err)
		}
		cancel()
		var ok, noChange, failed, clientTimeout atomic.Int64
		goroutines := runtime.NumGoroutine()
		before := server.transactions(t)
		started := time.Now()
		var wg sync.WaitGroup
		for index := 0; index < waiters; index++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				status, err := server.request(ctx, client, http.MethodPost, "/v1/machines/"+server.machines[index%len(server.machines)]+"/events/wait", body)
				switch {
				case err != nil && strings.Contains(err.Error(), "timeout awaiting response headers"):
					clientTimeout.Add(1)
				case err != nil || status >= 500:
					failed.Add(1)
				case status == http.StatusNoContent:
					noChange.Add(1)
				default:
					ok.Add(1)
				}
			}()
		}
		time.Sleep(10 * time.Second)
		peak := runtime.NumGoroutine() - goroutines
		wg.Wait()
		elapsed := time.Since(started)
		queries := server.transactions(t) - before
		t.Logf("waiters=%d: 204=%d 200=%d client-timeout=%d failed=%d in %v; %.0f DB transactions/s (%.2f per waiter per s); +%d goroutines held",
			waiters, noChange.Load(), ok.Load(), clientTimeout.Load(), failed.Load(), elapsed.Round(time.Millisecond),
			float64(queries)/elapsed.Seconds(), float64(queries)/elapsed.Seconds()/float64(waiters), peak)
		if clientTimeout.Load() > 0 {
			t.Errorf("waiters=%d: %d waits outlived the client's 30 s header timeout (the hold must end before it)", waiters, clientTimeout.Load())
		}
		if failed.Load() > 0 {
			t.Errorf("waiters=%d: %d waits failed", waiters, failed.Load())
		}
		client.CloseIdleConnections()
	}
}

// The requests one daemon's inventory refresh makes today (prod mix 2026-10-23), with the solo actor's data.
var refreshMix = []string{
	"/v1/machines?limit=200&offset=0&sort=alias",
	"/v1/machines?limit=200&offset=0&sort=alias",
	"/v1/workspaces",
	"/v1/machine-services",
	"/v1/previews?limit=200",
	"/v1/terminal-sessions?owner=mine&live=false&limit=200&offset=0",
	"/v1/client-configuration",
}

func TestLoadIdlePollMix(t *testing.T) {
	server := startLoadServer(t)
	// 15 s is today's interval with the event stream unhealthy; 300 s is the fallback once it is healthy.
	refreshEvery := time.Duration(loadSteps("PAPERBOAT_LOAD_REFRESH_SECONDS", []int{15})[0]) * time.Second
	window := max(60*time.Second, refreshEvery)
	for _, daemons := range loadSteps("PAPERBOAT_LOAD_DAEMONS", []int{50, 100, 250, 500, 1000}) {
		client := loadClient(256)
		var mu sync.Mutex
		latencies := map[string][]time.Duration{}
		statuses := map[int]int{}
		var transportErrors atomic.Int64
		ctx, cancel := context.WithTimeout(context.Background(), window)
		before := server.transactions(t)
		busyBefore, totalBefore := hostCPU()
		var wg sync.WaitGroup
		for daemon := 0; daemon < daemons; daemon++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Spread the daemons' refreshes over the interval like a steady fleet.
				offset := time.Duration(daemon) * refreshEvery / time.Duration(daemons)
				select {
				case <-ctx.Done():
					return
				case <-time.After(offset):
				}
				for ctx.Err() == nil {
					next := time.Now().Add(refreshEvery)
					for _, path := range refreshMix {
						requestCtx, cancelRequest := context.WithTimeout(context.Background(), 30*time.Second)
						started := time.Now()
						status, err := server.request(requestCtx, client, http.MethodGet, path, "")
						cancelRequest()
						key := strings.SplitN(path, "?", 2)[0]
						mu.Lock()
						if err != nil {
							transportErrors.Add(1)
						} else {
							statuses[status]++
							latencies[key] = append(latencies[key], time.Since(started))
						}
						mu.Unlock()
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Until(next)):
					}
				}
			}()
		}
		wg.Wait()
		cancel()
		busyAfter, totalAfter := hostCPU()
		cores := float64(runtime.NumCPU()) * float64(busyAfter-busyBefore) / float64(max(1, totalAfter-totalBefore))
		total, failed := 0, int(transportErrors.Load())
		for status, count := range statuses {
			total += count
			if status >= 500 || status == http.StatusTooManyRequests {
				failed += count
			}
		}
		var worst time.Duration
		var lines []string
		for path, values := range latencies {
			slices.Sort(values)
			p50, p99 := values[len(values)/2], values[(len(values)*99)/100]
			worst = max(worst, p99)
			lines = append(lines, fmt.Sprintf("%s p50 %v p99 %v", path, p50.Round(time.Millisecond), p99.Round(time.Millisecond)))
		}
		slices.Sort(lines)
		t.Logf("daemons=%d refresh=%v: %d requests (%.0f/s), %d failed, statuses %v, %.0f DB transactions/s; host busy %.2f of %d vCPU (server + PostgreSQL) = %.0f machines per busy vCPU\n  %s",
			daemons, refreshEvery, total+int(transportErrors.Load()), float64(total)/window.Seconds(), failed, statuses,
			float64(server.transactions(t)-before)/window.Seconds(), cores, runtime.NumCPU(), float64(daemons)/max(cores, 0.01), strings.Join(lines, "\n  "))
		client.CloseIdleConnections()
		if worst > 5*time.Second || failed*100 > total {
			t.Logf("first degradation at daemons=%d (p99 %v, %d failed)", daemons, worst.Round(time.Millisecond), failed)
			break
		}
	}
}

// TestLoadSeedOnly migrates and seeds the perf database and the load session, then exits: the multi-instance runner
// calls it before starting server instances on the same database.
func TestLoadSeedOnly(t *testing.T) {
	t.Setenv("PAPERBOAT_LOAD_TARGET_URL", "http://seed-only.example.test")
	server := startLoadServer(t)
	t.Logf("seeded: %d online solo machines", len(server.machines))
}

// TestLoadFailover is the horizontal-scaling check: server instances behind a proxy (PAPERBOAT_LOAD_TARGET_URL),
// held event waits plus the daemon refresh mix, and PAPERBOAT_LOAD_KILL_COMMAND run once mid-test to kill one
// instance. Each simulated daemon behaves like the real one: a failed request is retried after a backoff, a failed
// wait is re-issued after a backoff. Reports, per second, the requests that failed and how long the failure window
// lasted. Pass: errors only inside one short window after the kill, every wait loop back to 204/200 after it.
func TestLoadFailover(t *testing.T) {
	server := startLoadServer(t)
	kill := os.Getenv("PAPERBOAT_LOAD_KILL_COMMAND")
	if os.Getenv("PAPERBOAT_LOAD_TARGET_URL") == "" || kill == "" {
		t.Skip("set PAPERBOAT_LOAD_TARGET_URL (proxy in front of several instances) and PAPERBOAT_LOAD_KILL_COMMAND")
	}
	daemons := loadSteps("PAPERBOAT_LOAD_DAEMONS", []int{200})[0]
	const duration, killAt, backoff = 90 * time.Second, 30 * time.Second, 2 * time.Second
	client := loadClient(512)
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var mu sync.Mutex
	failures := map[int]int{} // second since start → failed requests
	successes := map[int]int{}
	record := func(ok bool) {
		second := int(time.Since(started) / time.Second)
		mu.Lock()
		if ok {
			successes[second]++
		} else {
			failures[second]++
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	body := `{"topics":{"inventory":0,"machine_services":0,"peer_network":0}}`
	for daemon := 0; daemon < daemons; daemon++ {
		machine := server.machines[daemon%len(server.machines)]
		wg.Add(2)
		go func() { // the held event wait
			defer wg.Done()
			for ctx.Err() == nil {
				requestCtx, cancelRequest := context.WithTimeout(ctx, 40*time.Second)
				status, err := server.request(requestCtx, client, http.MethodPost, "/v1/machines/"+machine+"/events/wait", body)
				cancelRequest()
				if ctx.Err() != nil {
					return
				}
				ok := err == nil && (status == http.StatusOK || status == http.StatusNoContent)
				record(ok)
				if !ok {
					time.Sleep(backoff)
				}
			}
		}()
		go func() { // the refresh mix every 15 s, retried after a backoff on failure
			defer wg.Done()
			time.Sleep(time.Duration(daemon) * 15 * time.Second / time.Duration(daemons))
			for ctx.Err() == nil {
				next := time.Now().Add(15 * time.Second)
				for _, path := range refreshMix {
					for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
						requestCtx, cancelRequest := context.WithTimeout(ctx, 30*time.Second)
						status, err := server.request(requestCtx, client, http.MethodGet, path, "")
						cancelRequest()
						ok := err == nil && status < 500
						if ctx.Err() == nil {
							record(ok)
						}
						if ok {
							break
						}
						time.Sleep(backoff)
					}
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Until(next)):
				}
			}
		}()
	}
	time.Sleep(killAt)
	output, err := exec.Command("/bin/sh", "-c", kill).CombinedOutput()
	t.Logf("t=%v: kill command: err=%v output=%q", killAt, err, strings.TrimSpace(string(output)))
	wg.Wait()
	first, last, total := -1, -1, 0
	for second := 0; second <= int(duration/time.Second); second++ {
		if failures[second] > 0 {
			if first < 0 {
				first = second
			}
			last = second
			total += failures[second]
		}
	}
	window := 0
	if first >= 0 {
		window = last - first + 1
	}
	afterKill, afterWindow := 0, 0
	for second, count := range successes {
		if second >= int(killAt/time.Second) {
			afterKill += count
		}
		if second > last {
			afterWindow += count
		}
	}
	t.Logf("daemons=%d: %d failed requests, failure window %ds (first at t=%ds, last at t=%ds); %d successes after the kill, %d after the window",
		daemons, total, window, first, last, afterKill, afterWindow)
	if window > int(2*backoff/time.Second)+5 {
		t.Errorf("failure window %ds exceeds the clients' retry backoff (%v) plus 5 s", window, backoff)
	}
	if first >= 0 && first < int(killAt/time.Second) {
		t.Errorf("requests failed before the kill (t=%ds)", first)
	}
	if afterWindow == 0 {
		t.Errorf("no request succeeded after the failure window")
	}
}

func loadSteps(name string, fallback []int) []int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	var steps []int
	for _, field := range strings.Split(value, ",") {
		step, err := strconv.Atoi(strings.TrimSpace(field))
		if err == nil && step > 0 {
			steps = append(steps, step)
		}
	}
	return steps
}

// hostCPU returns busy and total jiffies from /proc/stat. The staging VM runs only the server and its database, so
// host busy time is their cost, which is what "machines per vCPU" divides by. The load generator runs in this process
// too, so the figure is a conservative lower bound on machines per vCPU.
func hostCPU() (busy, total uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(strings.SplitN(string(data), "\n", 2)[0])
	for index, field := range fields[1:] {
		value, _ := strconv.ParseUint(field, 10, 64)
		total += value
		if index != 3 && index != 4 { // idle, iowait
			busy += value
		}
	}
	return busy, total
}
