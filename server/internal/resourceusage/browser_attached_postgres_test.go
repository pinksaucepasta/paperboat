package resourceusage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// A browser terminal counts toward the plan's open browser terminals only while attached (user rule):
// closing it frees the slot at once, though its admission has not expired.
func TestClosedBrowserTerminalFreesItsSlotAtOncePostgres(t *testing.T) {
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
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	limits := config.Default().Billing.Limits
	limits.BrowserTerminals.Free = 1
	planlimits.Configure(limits, "")
	t.Cleanup(func() { planlimits.Configure(config.Default().Billing.Limits, "") })
	x := fmt.Sprint(time.Now().UnixNano())
	user, machine := "ba_u_"+x, "ba_m_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$1,'ba','linux','amd64','/w','online','occupied',true)`, machine, user)
	for _, term := range []string{"ba_t1_" + x, "ba_t2_" + x} {
		exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd) VALUES($1,$2,$3,$1,'/w')`, term, machine, user)
	}
	usage := New(store)
	scope := planlimits.Scope{Account: user}
	open := func(terminal string) error {
		t.Helper()
		var out error
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			out = planlimits.CheckBrowserTerminalTx(ctx, tx, scope, terminal)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := usage.RecordBrowserAdmission(ctx, "ba_a1_"+x, machine, "ba_t1_"+x, "node_ba", user, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := open("ba_t2_" + x); !errors.Is(err, planlimits.ErrLimitReached) {
		t.Fatalf("second terminal while the first is attached: %v", err)
	}
	if err := usage.RecordBrowserDetached(ctx, "ba_a1_"+x); err != nil {
		t.Fatal(err)
	}
	if err := open("ba_t2_" + x); err != nil {
		t.Fatalf("a closed browser terminal still counted: %v", err)
	}
	if err := usage.RecordBrowserAttached(ctx, "ba_a1_"+x, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := open("ba_t2_" + x); err != nil {
		t.Fatalf("a renewal revived a closed browser terminal: %v", err)
	}
}

// Free allows 10 open browser terminals. Opening and closing 10 at once frees all 10 slots at once: the
// 11th opens immediately. With 10 open, the 11th is refused with the typed limit error.
func TestTenBrowserTerminalsOpenAndClosedOnFreePostgres(t *testing.T) {
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
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	planlimits.Configure(config.Default().Billing.Limits, "")
	x := fmt.Sprint(time.Now().UnixNano())
	user, machine := "bt_u_"+x, "bt_m_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$1,'bt','linux','amd64','/w','online','occupied',true)`, machine, user)
	for i := 0; i <= 10; i++ {
		exec(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd) VALUES($1,$2,$3,$1,'/w')`, fmt.Sprintf("bt_t%d_%s", i, x), machine, user)
	}
	usage := New(store)
	scope := planlimits.Scope{Account: user}
	canOpen := func(i int) error {
		t.Helper()
		var out error
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			out = planlimits.CheckBrowserTerminalTx(ctx, tx, scope, fmt.Sprintf("bt_t%d_%s", i, x))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	attach := func(i int) {
		t.Helper()
		if err := canOpen(i); err != nil {
			t.Fatalf("terminal %d refused below the limit: %v", i, err)
		}
		if err := usage.RecordBrowserAdmission(ctx, fmt.Sprintf("bt_a%d_%s", i, x), machine, fmt.Sprintf("bt_t%d_%s", i, x), "node_bt", user, time.Now().Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		attach(i)
	}
	// 10 open: the 11th is refused with the typed plan-limit error (10 of 10).
	var limit *planlimits.LimitError
	if err := canOpen(10); !errors.As(err, &limit) || limit.Used != 10 || limit.Limit != 10 || limit.Code() != "browser_terminal_limit_reached" {
		t.Fatalf("11th terminal with 10 open: %v", err)
	}
	// Close all 10 at once: the 11th opens immediately.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := usage.RecordBrowserDetached(ctx, fmt.Sprintf("bt_a%d_%s", i, x)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if err := canOpen(10); err != nil {
		t.Fatalf("11th terminal after the 10 closed: %v", err)
	}
}
