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

// The machine list allocated each machine's local address lease inside its query, under a table-wide lock: the first
// list of a 100-machine account ran 29 s and every other list of it waited on Lock:relation (Lane W staging). The list
// only reads leases now, and allocation takes no table lock: concurrent allocations do not wait for each other, a
// released slot is reused, and slots never collide.
func TestMachineListAllocatesNoLeaseAndAllocationTakesNoTableLockOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprintf("lease_user_%d", time.Now().UnixNano())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
SELECT $1||'_m'||g,$1,$1||'_e'||g,'m-'||g,'linux','amd64','/w','online','occupied',true FROM generate_series(1,1000) g`, user)
	leases := func() int {
		t.Helper()
		var n int
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.control_sync_leases l JOIN paperboat.user_machines m ON m.id=l.machine_id WHERE m.user_id=$1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	started := time.Now()
	items, total, _, err := New(store, nil, Policy{}, nil).ListInventory(workspace.WithContext(ctx, workspace.Scope{ID: "personal"}), user, InventoryOptions{Limit: 200})
	if err != nil || len(items) != 200 || total != 1000 {
		t.Fatalf("first list: %d items, total %d, %v", len(items), total, err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the first list of a 1,000-machine account took %v", took)
	}
	if n := leases(); n != 0 {
		t.Fatalf("listing machines allocated %d address leases; only a control sync connection allocates one", n)
	}

	// One allocation's transaction stays open; another allocation must not wait for it.
	holder, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	var first int
	if err := holder.QueryRow(ctx, `SELECT address_slot FROM paperboat.ensure_control_sync_lease($1)`, user+"_m1").Scan(&first); err != nil {
		t.Fatal(err)
	}
	other, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(context.Background())
	if _, err := other.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		t.Fatal(err)
	}
	var second int
	if err := other.QueryRow(ctx, `SELECT address_slot FROM paperboat.ensure_control_sync_lease($1)`, user+"_m2").Scan(&second); err != nil {
		t.Fatalf("an allocation while another one's transaction is open: %v (it waited on a table lock)", err)
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("both allocations commit: %v", err)
	}
	if first == second {
		t.Fatalf("two machines got slot %d", first)
	}
	// A slot released by a deleted machine goes to the next machine once its quarantine is over.
	exec(`UPDATE paperboat.control_sync_leases SET machine_id=NULL WHERE address_slot=$1`, second)
	exec(`UPDATE paperboat.control_sync_leases SET released_at=now()-interval '8 days' WHERE address_slot=$1`, second)
	var reused int
	if err := store.SQL().QueryRowContext(ctx, `SELECT address_slot FROM paperboat.ensure_control_sync_lease($1)`, user+"_m3").Scan(&reused); err != nil || reused != second {
		t.Fatalf("after slot %d was released the next allocation got %d (%v), want the released slot", second, reused, err)
	}
	if again := func() int {
		var slot int
		if err := store.SQL().QueryRowContext(ctx, `SELECT address_slot FROM paperboat.ensure_control_sync_lease($1)`, user+"_m3").Scan(&slot); err != nil {
			t.Fatal(err)
		}
		return slot
	}(); again != reused {
		t.Fatalf("a machine's lease changed on a second call: %d -> %d", reused, again)
	}
}

// The local address pool is 127.64.0.0/10 (migration 172): 127.100/16 fills first and the next address is
// 127.101.0.1 (last octets 255 and 0 are never handed out); a deleted machine's address waits out the quarantine
// before another machine gets it; a full pool answers 54000; 1,000 machines allocating at once get distinct
// addresses without an error.
func TestMachineAddressPoolOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprintf("pool_user_%d", time.Now().UnixNano())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
SELECT $1||'_m'||g,$1,$1||'_e'||g,'m-'||g,'linux','amd64','/w','online','occupied',true FROM generate_series(1,1003) g`, user)
	// Later tests on the same database allocate in 127.100/16 again.
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_sync_leases WHERE address_slot>=65534 OR machine_id LIKE $1||'_m%'`, user)
		_, _ = store.SQL().ExecContext(context.Background(), `UPDATE paperboat.machine_address_pool SET size=4194304`)
	})
	allocate := func(machine string) (int, error) {
		var slot int
		err := store.SQL().QueryRowContext(ctx, `SELECT address_slot FROM paperboat.ensure_control_sync_lease($1)`, machine).Scan(&slot)
		return slot, err
	}
	// 127.100.255.254 is the last address of the old pool; it is released, inside its quarantine.
	exec(`INSERT INTO paperboat.control_sync_leases(machine_id,address_slot) VALUES($1,65534)`, user+"_m1")
	exec(`UPDATE paperboat.control_sync_leases SET machine_id=NULL WHERE address_slot=65534`)
	slot, err := allocate(user + "_m2")
	if err != nil || machineaddress.FromSlot(slot).String() != "127.101.0.1" {
		t.Fatalf("after 127.100.255.254: slot %d (%s), %v; want 127.101.0.1", slot, machineaddress.FromSlot(slot), err)
	}
	// After the quarantine the released address is handed out again.
	exec(`UPDATE paperboat.control_sync_leases SET released_at=now()-interval '8 days' WHERE address_slot=65534`)
	if slot, err = allocate(user + "_m3"); err != nil || slot != 65534 {
		t.Fatalf("after the quarantine: slot %d, %v; want the released 65534", slot, err)
	}
	// A full pool.
	exec(`UPDATE paperboat.machine_address_pool SET size=65536`)
	if _, err = allocate(user + "_m4"); err == nil || !strings.Contains(err.Error(), "54000") {
		t.Fatalf("allocation in a full pool: %v, want 54000", err)
	}
	exec(`UPDATE paperboat.machine_address_pool SET size=4194304`)

	// 1,000 machines at once.
	slots := make([]int, 1000)
	errs := make([]error, len(slots))
	var workers sync.WaitGroup
	gate := make(chan struct{}, 16)
	for i := range slots {
		workers.Add(1)
		gate <- struct{}{}
		go func() {
			defer func() { <-gate; workers.Done() }()
			slots[i], errs[i] = allocate(fmt.Sprintf("%s_m%d", user, i+4))
		}()
	}
	workers.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent allocation: %v", err)
		}
	}
	seen := map[int]bool{}
	for _, slot := range slots {
		if seen[slot] || slot <= 65537 {
			t.Fatalf("slot %d handed out twice or below the pool's highest", slot)
		}
		seen[slot] = true
	}
}
