package usermachines

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineaddress"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

type leaseFixture struct {
	t     *testing.T
	ctx   context.Context
	store *db.DB
	exec  func(query string, args ...any)
}

func newLeaseFixture(t *testing.T) leaseFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	return leaseFixture{t: t, ctx: ctx, store: store, exec: func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}}
}

// account makes an active user with n machines <user>_m1..n.
func (f leaseFixture) account(user string, n int) {
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
SELECT $1||'_m'||g,$1,$1||'_e'||g,'m-'||g,'linux','amd64','/w','online','occupied',true FROM generate_series(1,$2) g`, user, n)
	// The machines (up to 65,024 in the exhaustion test) would otherwise stay in the shared test database and slow every
	// later test (the team machine list timed out at 30 s). Triggers and cascades are off for this delete only.
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		err := f.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				return err
			}
			for _, statement := range []string{
				`DELETE FROM paperboat.control_sync_leases WHERE account_id=$1 OR machine_id LIKE $1||'\_m%'`,
				`DELETE FROM paperboat.user_machines WHERE user_id=$1`,
				`DELETE FROM paperboat.users WHERE id=$1`,
			} {
				if _, err := tx.Exec(ctx, statement, user); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			f.t.Errorf("lease fixture cleanup of %s: %v", user, err)
		}
	})
}

// slots allocates (or reads) the account's leases for machines and returns machine -> slot.
func (f leaseFixture) slots(t *testing.T, account string, machines ...string) map[string]int {
	t.Helper()
	rows, err := f.store.SQL().QueryContext(f.ctx, `SELECT leased_machine,leased_slot FROM paperboat.ensure_machine_address_leases($1,$2::text[])`, account, machines)
	if err != nil {
		t.Fatalf("ensure leases of %s: %v", account, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var machine string
		var slot int
		if err := rows.Scan(&machine, &slot); err != nil {
			t.Fatal(err)
		}
		out[machine] = slot
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Machine addresses are per account (PLAN 3b "Multi-account hosts", migration 199): the machine list only reads leases
// (allocating inside it took a table lock per list: 29 s for a 100-machine account), two accounts get the same slot
// number, a clash inside an account is resolved by the next free slot of that account, 200 allocations at once in one
// account get distinct slots, one account's open allocation does not hold up another's, and a deleted machine's slot is
// free again at once (no quarantine).
func TestMachineAddressLeasesArePerAccountOnPostgres(t *testing.T) {
	f := newLeaseFixture(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	a, b := "lease_a_"+suffix, "lease_b_"+suffix
	f.account(a, 1000)
	f.account(b, 2)

	started := time.Now()
	items, total, _, err := New(f.store, nil, Policy{}, nil).ListInventory(workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"}), a, InventoryOptions{Limit: 200})
	if err != nil || len(items) != 200 || total != 1000 || time.Since(started) > 5*time.Second {
		t.Fatalf("first list of a 1,000-machine account: %d items, total %d, %v after %v", len(items), total, err, time.Since(started))
	}
	var leases int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.control_sync_leases WHERE account_id=$1`, a).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("listing allocated %d leases (%v); only pairing and a viewer's first sight of an approved machine does", leases, err)
	}

	// Two accounts, the same slot number: the lowest free slot of each, 127.100.0.2.
	first := f.slots(t, a, a+"_m1")[a+"_m1"]
	other := f.slots(t, b, b+"_m1")[b+"_m1"]
	if first != 2 || other != 2 || machineaddress.FromSlot(first).String() != "127.100.0.2" {
		t.Fatalf("first leases: account A slot %d, account B slot %d; want both 2 (127.100.0.2)", first, other)
	}
	// A clash inside an account is resolved by that account: its next machine gets the next free slot, the same
	// machine keeps its slot, and the other account is not touched.
	if got := f.slots(t, a, a+"_m1", a+"_m2"); got[a+"_m1"] != 2 || got[a+"_m2"] != 3 {
		t.Fatalf("account A after a second machine: %v, want m1=2 m2=3", got)
	}
	if got := f.slots(t, b, b+"_m1", b+"_m2"); got[b+"_m1"] != 2 || got[b+"_m2"] != 3 {
		t.Fatalf("account B: %v, want m1=2 m2=3 (its own numbering)", got)
	}
	// The slot of a machine another account owns is not the viewer's until the viewer allocates its own.
	if got := f.slots(t, b, a+"_m1"); got[a+"_m1"] != 4 {
		t.Fatalf("account B's lease for A's machine: %v, want its next free slot 4", got)
	}

	// 200 allocations at once in one account: distinct slots, no error.
	machines := make([]string, 0, 200)
	for i := 3; i <= 202; i++ {
		machines = append(machines, fmt.Sprintf("%s_m%d", a, i))
	}
	var wg sync.WaitGroup
	results := make(chan map[string]int, len(machines))
	for _, machine := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := f.store.SQL().QueryContext(f.ctx, `SELECT leased_machine,leased_slot FROM paperboat.ensure_machine_address_leases($1,ARRAY[$2]::text[])`, a, machine)
			if err != nil {
				results <- nil
				t.Errorf("concurrent allocation for %s: %v", machine, err)
				return
			}
			defer rows.Close()
			got := map[string]int{}
			for rows.Next() {
				var m string
				var s int
				if err := rows.Scan(&m, &s); err == nil {
					got[m] = s
				}
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	seen := map[int]string{}
	for got := range results {
		for machine, slot := range got {
			if previous, taken := seen[slot]; taken {
				t.Fatalf("slot %d went to both %s and %s", slot, previous, machine)
			}
			seen[slot] = machine
		}
	}
	if len(seen) != len(machines) {
		t.Fatalf("%d of %d concurrent allocations returned a slot", len(seen), len(machines))
	}

	// One account's allocation transaction stays open; another account's must not wait for it.
	holder, err := f.store.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(f.ctx, `SELECT paperboat.ensure_machine_address_leases($1,ARRAY[$2]::text[])`, a, a+"_m500"); err != nil {
		t.Fatal(err)
	}
	other2, err := f.store.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other2.Rollback(context.Background())
	if _, err := other2.Exec(f.ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := other2.Exec(f.ctx, `SELECT paperboat.ensure_machine_address_leases($1,ARRAY[$2]::text[])`, b, b+"_m2"); err != nil {
		t.Fatalf("an allocation of another account while one is open: %v (it waited)", err)
	}
	if err := other2.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}

	// A deleted machine's slot is free again at once, and its lease went with it.
	f.exec(`DELETE FROM paperboat.user_machines WHERE id=$1`, a+"_m1")
	var remaining int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.control_sync_leases WHERE machine_id=$1`, a+"_m1").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("leases left for a deleted machine: %d, %v (account B's lease for it should go too)", remaining, err)
	}
	if got := f.slots(t, a, a+"_m900"); got[a+"_m900"] != 2 {
		t.Fatalf("after machine m1 was deleted the next allocation got %v, want its freed slot 2", got)
	}
}

// An account has 65,023 addresses (127.100.0.2 .. 127.100.255.254 without the .0 and .255 hosts); the next machine
// gets 54000 (program_limit_exceeded), while another account is unaffected.
func TestMachineAddressLeasesRunOutPerAccountOnPostgres(t *testing.T) {
	f := newLeaseFixture(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	full, free := "lease_full_"+suffix, "lease_free_"+suffix
	f.account(full, 65024)
	f.account(free, 1)
	// The bulk insert is one account's whole pool; the per-row publish triggers are not what this test measures.
	f.exec(`ALTER TABLE paperboat.control_sync_leases DISABLE TRIGGER USER`)
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(context.Background(), `ALTER TABLE paperboat.control_sync_leases ENABLE TRIGGER USER`)
	})
	f.exec(`INSERT INTO paperboat.control_sync_leases(account_id,machine_id,address_slot)
SELECT $1,$1||'_m'||n,s FROM (SELECT s,row_number() OVER (ORDER BY s) n FROM generate_series(2,65534) s WHERE s%256 BETWEEN 1 AND 254) x WHERE n<=65023`, full)
	f.exec(`ALTER TABLE paperboat.control_sync_leases ENABLE TRIGGER USER`)
	_, err := f.store.SQL().ExecContext(f.ctx, `SELECT * FROM paperboat.ensure_machine_address_leases($1,ARRAY[$2]::text[])`, full, full+"_m65024")
	if err == nil || !strings.Contains(err.Error(), "54000") {
		t.Fatalf("the 65,024th address of an account: %v, want 54000", err)
	}
	if got := f.slots(t, free, free+"_m1"); got[free+"_m1"] != 2 {
		t.Fatalf("another account while one is full: %v", got)
	}
}
