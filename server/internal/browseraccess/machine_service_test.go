package browseraccess

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAutomaticBrowserDormantAuthorityPostgres(t *testing.T) {
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
	defer store.Close()
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, other, machine, environment := "ab-owner-"+suffix, "ab-other-"+suffix, "ab-machine-"+suffix, "ab-env-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active'),($2,$2,$2||'@invalid.test','active')`, owner, other)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'office','linux','amd64','/workspace','offline','occupied',false,1)`, machine, owner, environment)
	exec(`UPDATE paperboat.user_machines SET seat_state='occupied' WHERE id=$1`, machine)
	reserve := func(port int) string {
		t.Helper()
		tx, err := store.SQL().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		host, err := machineservices.ReserveBrowserHostname(ctx, tx, machine, port, "preview.example.test")
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return host
	}
	host := reserve(3000)
	now := time.Now().UTC()
	resolve := func(actor, hostname string) (authority, error) {
		var out authority
		e := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var err error
			out, err = resolveAuthority(ctx, tx, hostname, actor, now, "preview.example.test")
			return err
		})
		return out, e
	}
	for _, scenario := range []struct{ actor, host string }{{other, host}, {owner, "not-allocated.example.test"}, {owner, strings.TrimSuffix(host, ".preview.example.test") + ".attacker.test"}} {
		if _, e = resolve(scenario.actor, scenario.host); !errors.Is(e, ErrNotAuthorized) {
			t.Fatalf("unauthorized resolution: %v", e)
		}
	}
	var count int
	if e = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.lazy_access_policies WHERE machine_id=$1`, machine).Scan(&count); e != nil || count != 0 {
		t.Fatalf("unauthorized materialization %d %v", count, e)
	}
	a, e := resolve(owner, host)
	if e != nil || a.kind != "lazy_policy" || a.machine != machine || a.previewOwnerSession == "" {
		t.Fatalf("authorized dormant resolution %+v %v", a, e)
	}
	if e = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.lazy_activations WHERE policy_id=$1`, a.id).Scan(&count); e != nil || count != 0 {
		t.Fatalf("dormant auth activated target %d %v", count, e)
	}
	exec(`UPDATE paperboat.user_machines SET personal_auto_enabled=false,machine_service_policy_generation=machine_service_policy_generation+1 WHERE id=$1`, machine)
	if _, e = resolve(owner, host); !errors.Is(e, ErrNotAuthorized) {
		t.Fatalf("auto off %v", e)
	}
	exec(`UPDATE paperboat.user_machines SET personal_auto_enabled=true,machine_service_policy_generation=machine_service_policy_generation+1 WHERE id=$1`, machine)
	newer, e := resolve(owner, host)
	if e != nil || newer.previewOwnerSession == a.previewOwnerSession {
		t.Fatalf("old authority fingerprint reused %+v %v", newer, e)
	}
	snapshot := machineservices.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-browser-family-0123", StartedAt: time.Now().UTC(), Generation: 1, Services: []machineservices.Port{{Port: 3000, AddressFamily: "ipv6"}, {Port: 3001, AddressFamily: "ipv6"}}}
	discovery := machineservices.New(store, "local.example.test")
	if e = discovery.RecordMachineServices(ctx, environment, machine, snapshot, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	var address string
	var generation int64
	if e = store.SQL().QueryRowContext(ctx, `SELECT target_address,generation FROM paperboat.lazy_access_policies WHERE id=$1`, a.id).Scan(&address, &generation); e != nil || address != "[::1]:3000" || generation <= a.generation {
		t.Fatalf("family replacement did not fence browser target: %s %d %v", address, generation, e)
	}
	ipv6, e := resolve(owner, reserve(3001))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.SQL().QueryRowContext(ctx, `SELECT target_address FROM paperboat.lazy_access_policies WHERE id=$1`, ipv6.id).Scan(&address); e != nil || address != "[::1]:3001" {
		t.Fatalf("new IPv6 target: %s %v", address, e)
	}
	exec(`UPDATE paperboat.lazy_access_policies SET authorization_source='explicit',target_address='127.0.0.1:3000' WHERE id=$1`, a.id)
	snapshot.Generation++
	if e = discovery.RecordMachineServices(ctx, environment, machine, snapshot, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	if e = store.SQL().QueryRowContext(ctx, `SELECT target_address FROM paperboat.lazy_access_policies WHERE id=$1`, a.id).Scan(&address); e != nil || address != "127.0.0.1:3000" {
		t.Fatalf("discovery overwrote explicit family: %s %v", address, e)
	}

}
