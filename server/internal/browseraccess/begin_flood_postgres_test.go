package browseraccess

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func beginFloodService(t *testing.T, limits config.RateLimits) (*Service, *db.DB, string) {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if e := db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(store, "https://console.example.test")
	if e != nil {
		t.Fatal(e)
	}
	svc.MachineServiceDomain = "preview.example.test"
	svc.ConfigureBegin(limits)
	suffix := fmt.Sprint(time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.browser_access_transactions WHERE hostname LIKE '%'||$1||'.preview.example.test'`, suffix)
	})
	return svc, store, suffix
}

func countBeginRows(t *testing.T, store *db.DB, suffix string) (mine, all int) {
	t.Helper()
	if e := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FILTER (WHERE hostname LIKE '%'||$1||'.preview.example.test'), count(*) FROM paperboat.browser_access_transactions`, suffix).Scan(&mine, &all); e != nil {
		t.Fatal(e)
	}
	return mine, all
}

// A flood of random names from many addresses must not grow the table past its cap: the oldest rows go first, so the
// newest sign-in survives and the first is gone.
func TestBeginFloodOfRandomHostsStaysBoundedPostgres(t *testing.T) {
	svc, store, suffix := beginFloodService(t, config.RateLimits{BrowserBeginPerMinute: 1000000, BrowserBeginBurst: 1000000, BrowserBeginGlobalPerMinute: 1000000, BrowserBeginGlobalBurst: 1000000, BrowserBeginMaxLive: 300, MaxClients: 100000})
	ctx := context.Background()
	const flood = 700
	var first, last BeginResult
	for i := 0; i < flood; i++ {
		out, e := svc.Begin(ctx, BeginRequest{Host: fmt.Sprintf("flood-%d-%s.preview.example.test", i, suffix), ReturnPath: "/", ClientIP: fmt.Sprintf("198.51.%d.%d", i/250, i%250)})
		if e != nil {
			t.Fatalf("begin %d: %v", i, e)
		}
		if i == 0 {
			first = out
		}
		last = out
	}
	mine, all := countBeginRows(t, store, suffix)
	if all > svc.maxLive || mine < 1 {
		t.Fatalf("after %d random names the table holds %d rows (%d ours), cap %d", flood, all, mine, svc.maxLive)
	}
	var firstKept, lastKept bool
	if e := store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.browser_access_transactions WHERE transaction_id=$1), EXISTS(SELECT 1 FROM paperboat.browser_access_transactions WHERE transaction_id=$2)`, first.TransactionID, last.TransactionID).Scan(&firstKept, &lastKept); e != nil {
		t.Fatal(e)
	}
	if firstKept || !lastKept {
		t.Fatalf("eviction is not oldest-first: first kept=%v last kept=%v", firstKept, lastKept)
	}
	// A cap lowered under a full table comes down on the next begin, not after hundreds of them.
	svc.maxLive = 100
	if _, e := svc.Begin(ctx, BeginRequest{Host: "lowered-" + suffix + ".preview.example.test", ReturnPath: "/", ClientIP: "192.0.2.1"}); e != nil {
		t.Fatal(e)
	}
	if _, all := countBeginRows(t, store, suffix); all > svc.maxLive {
		t.Fatalf("after the cap was lowered to %d the table holds %d rows", svc.maxLive, all)
	}
}

// Past a per-address or global limit nothing is stored, and the answer has the shape of a stored one: a flood
// cannot tell it was limited. A malformed request still gets its own refusal, which says nothing about limits.
func TestBeginRateLimitStoresNothingAndAnswersLikeAnyOtherPostgres(t *testing.T) {
	svc, store, suffix := beginFloodService(t, config.RateLimits{BrowserBeginPerMinute: 1, BrowserBeginBurst: 3, BrowserBeginGlobalPerMinute: 1, BrowserBeginGlobalBurst: 5, BrowserBeginMaxLive: 100000, MaxClients: 100000})
	ctx := context.Background()
	begin := func(i int, ip string) BeginResult {
		t.Helper()
		out, e := svc.Begin(ctx, BeginRequest{Host: fmt.Sprintf("limited-%d-%s.preview.example.test", i, suffix), ReturnPath: "/", ClientIP: ip})
		if e != nil || out.TransactionID == "" || out.State == "" || !out.ExpiresAt.After(time.Now()) || out.LoginOrigin != "https://console.example.test" {
			t.Fatalf("begin %d from %s: %+v %v", i, ip, out, e)
		}
		return out
	}
	for i := 0; i < 8; i++ {
		begin(i, "203.0.113.9")
	}
	if mine, _ := countBeginRows(t, store, suffix); mine != 3 {
		t.Fatalf("one address stored %d rows, want its burst of 3", mine)
	}
	// An IPv6 address counts as its /64, so rotating the low bits does not get a new allowance.
	for i := 10; i < 14; i++ {
		begin(i, fmt.Sprintf("2001:db8:1:2::%x", i))
	}
	if mine, _ := countBeginRows(t, store, suffix); mine != 5 {
		t.Fatalf("after one /64 and the global burst of 5 the table holds %d rows, want 5", mine)
	}
	if _, e := svc.Begin(ctx, BeginRequest{Host: "bad host", ReturnPath: "/", ClientIP: "203.0.113.9"}); e != ErrInvalid {
		t.Fatalf("malformed begin: %v, want ErrInvalid", e)
	}
}
