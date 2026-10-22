package app

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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/perf"
)

type perfQueryCounter struct{}

// Every endpoint in internal/perf/budgets.json, called through the real router as its seeded actor, stays within
// its median latency budget and its number of database queries per request. A latency miss is re-measured once
// so a burst on the shared host does not fail the gate; a real regression fails both times.
func TestPerformanceBudgetsOnSeededPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_PERF_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_PERF_DATABASE_DSN to an isolated *_test database (seeded on first use)")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	budgets, err := perf.LoadBudgets()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := config.Default()
	cfg.Database = config.Database{Driver: "postgres", DSN: dsn}
	seedStore, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	err = perf.Prepare(ctx, seedStore)
	seedStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.db.Close() })
	store := application.db
	store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		if counter, ok := ctx.Value(perfQueryCounter{}).(*atomic.Int64); ok {
			counter.Add(1)
		}
		return ctx, func(string) {}
	})
	tokens := map[string]string{}
	for _, actor := range []string{perf.TeamOwner, perf.Solo, perf.TeamMember} {
		token := "perf-session-" + actor
		sum := sha256.Sum256([]byte(token))
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at,workos_session_id,workos_access_expires_at)
VALUES($1,$2,$3,'perf',now()+interval '29 days','session_perf',now()+interval '1 day')
ON CONFLICT (id) DO UPDATE SET session_hash=excluded.session_hash,expires_at=excluded.expires_at,revoked_at=NULL,workos_access_expires_at=excluded.workos_access_expires_at`,
			"session_perf_"+actor, actor, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
		tokens[actor] = token
	}
	one := func(query string) string {
		var value string
		if err := store.SQL().QueryRowContext(ctx, query).Scan(&value); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return value
	}
	paths := strings.NewReplacer(
		"{team_machine}", one(`SELECT id FROM paperboat.user_machines WHERE owner_team_id='perf-team-001' AND state='online' ORDER BY id LIMIT 1`),
		"{solo_machine}", one(`SELECT id FROM paperboat.user_machines WHERE user_id='user_perf_0002' AND owner_team_id IS NULL AND state='online' ORDER BY id LIMIT 1`),
		"{solo_tunnel}", one(`SELECT id FROM paperboat.tunnels WHERE account_id='user_perf_0002' AND owner_team_id IS NULL AND desired_state='active' ORDER BY id LIMIT 1`),
		"{solo_preview}", one(`SELECT id FROM paperboat.preview_leases WHERE account_id='user_perf_0002' ORDER BY created_at DESC LIMIT 1`),
	)
	measure := func(endpoint perf.Endpoint) (p50, p95 time.Duration, queries int64, status int) {
		var durations []time.Duration
		for i := 0; i < budgets.Iterations+2; i++ {
			counter := &atomic.Int64{}
			requestCtx, cancel := context.WithTimeout(context.WithValue(ctx, perfQueryCounter{}, counter), 10*time.Second)
			request := httptest.NewRequest(http.MethodGet, paths.Replace(endpoint.Path), nil).WithContext(requestCtx)
			request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tokens[endpoint.Actor]})
			request.AddCookie(&http.Cookie{Name: auth.DevSessionCookieName, Value: tokens[endpoint.Actor]})
			recorder := httptest.NewRecorder()
			started := time.Now()
			application.server.Handler.ServeHTTP(recorder, request)
			elapsed := time.Since(started)
			cancel()
			if i >= 2 { // the first two warm caches and prepared statements
				durations = append(durations, elapsed)
			}
			queries, status = max(queries, counter.Load()), recorder.Code
		}
		slices.Sort(durations)
		return durations[len(durations)/2], durations[(len(durations)*95+99)/100-1], queries, status
	}
	var report []string
	for _, endpoint := range budgets.Endpoints {
		actor, ok := tokens[endpoint.Actor]
		if !ok || actor == "" {
			t.Fatalf("budget %s: unknown actor %q", endpoint.Path, endpoint.Actor)
		}
		budget := time.Duration(endpoint.P50Ms * float64(time.Millisecond))
		p50, p95, queries, status := measure(endpoint)
		if p50 > budget {
			p50, p95, queries, status = measure(endpoint)
		}
		report = append(report, fmt.Sprintf("%7.1f %7.1f %7.0f %3d/%-3d %d %s %s", ms(p50), ms(p95), endpoint.P50Ms, queries, endpoint.MaxQueries, status, endpoint.Actor, endpoint.Path))
		switch {
		case status != http.StatusOK:
			t.Errorf("%s %s: status %d, want 200 (a budget only means something for a successful read)", endpoint.Actor, endpoint.Path, status)
		case p50 > budget:
			t.Errorf("%s %s: median %.1f ms over its %.0f ms budget", endpoint.Actor, endpoint.Path, ms(p50), endpoint.P50Ms)
		case queries > int64(endpoint.MaxQueries):
			t.Errorf("%s %s: %d queries per request, budget %d (N+1?)", endpoint.Actor, endpoint.Path, queries, endpoint.MaxQueries)
		}
	}
	t.Logf("p50ms p95ms budget queries status actor path\n%s", strings.Join(report, "\n"))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
