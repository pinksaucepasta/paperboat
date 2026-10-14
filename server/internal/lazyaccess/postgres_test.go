package lazyaccess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
)

func TestPolicyPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, store); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "usr_lazy_" + suffix
	machine := "mach_lazy_" + suffix
	environment := "env_lazy_" + suffix
	now := time.Now().UTC().Truncate(time.Microsecond)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.lazy_access_policies WHERE account_id=$1`, owner)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@test.invalid','active')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,installation_generation) VALUES($1,$2,$3,$1,'linux','amd64','/workspace','online','occupied',4)`, machine, owner, environment); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(store, "preview.example.test")
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return now }
	in := UpsertRequest{MachineID: machine, Target: Target{Scheme: "http", Address: "127.0.0.1:3210"}, AccessMode: "private", ExpiresAt: now.Add(time.Hour)}
	p, err := svc.Upsert(ctx, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z]+-[0-9]{4}[.]preview[.]example[.]test$`).MatchString(p.Hostname) || p.InstallationGeneration != 4 || p.Generation != 1 {
		t.Fatalf("policy=%#v", p)
	}
	if got, err := svc.Get(ctx, owner, p.ID); err != nil || got.Hostname != p.Hostname {
		t.Fatalf("get=%#v err=%v", got, err)
	}
	svc.now = func() time.Time { return now.Add(2 * time.Hour) }
	if got, err := svc.Get(ctx, owner, p.ID); err != nil || got.Hostname != p.Hostname {
		t.Fatalf("expired reservation is not inspectable: %v", err)
	}
	svc.now = func() time.Time { return now }
	in.ID = p.ID
	in.ExpectedGeneration = 1
	in.AccessMode = "team"
	in.Target.Scheme = "https"
	updated, err := svc.Upsert(ctx, owner, in)
	if err != nil || updated.Generation != 2 || updated.Target.Scheme != "https" {
		t.Fatalf("update=%#v err=%v", updated, err)
	}
	if _, err = svc.Upsert(ctx, owner, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update err=%v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=5 WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Get(ctx, owner, p.ID); !errors.Is(err, ErrDenied) {
		t.Fatalf("old installation remained approved: %v", err)
	}
	in.ExpectedGeneration = 2
	reapproved, err := svc.Upsert(ctx, owner, in)
	if err != nil || reapproved.Generation != 3 || reapproved.InstallationGeneration != 5 || reapproved.Hostname != p.Hostname {
		t.Fatalf("reapproval=%#v err=%v", reapproved, err)
	}
	if err = svc.Delete(ctx, owner, p.ID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Get(ctx, owner, p.ID); !errors.Is(err, ErrDenied) {
		t.Fatalf("tombstone get err=%v", err)
	}
	var retained int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.lazy_access_policies WHERE id=$1 AND deleted_at IS NOT NULL`, p.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("tombstone=%d err=%v", retained, err)
	}
}

func TestPolicyRegrantPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, store); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "regrant-owner-"+suffix, "regrant-machine-"+suffix, "regrant-env-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@test.invalid','active')`, owner)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$1,$1,'regrant','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, owner)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation,personal_auto_enabled) VALUES($1,$2,$3,'regrant','linux','amd64','/workspace','online','occupied',true,1,false)`, machine, owner, environment)
	svc, err := NewService(store, "preview.example.test")
	if err != nil {
		t.Fatal(err)
	}
	input := UpsertRequest{MachineID: machine, Target: Target{Scheme: "http", Address: "127.0.0.1:80"}, AccessMode: "private", ExpiresAt: time.Now().Add(time.Hour)}
	first, err := svc.Upsert(ctx, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	services := machineservices.New(store)
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot := machineservices.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-regrant-" + suffix, StartedAt: now.Add(-time.Minute), Generation: 1, Services: []machineservices.Port{{Port: 80, AddressFamily: "ipv4"}}}
	if err = services.RecordMachineServices(ctx, environment, machine, snapshot, now); err != nil {
		t.Fatal(err)
	}
	resolver, err := nativeprivateaccess.NewSQLResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	request := nativeprivateaccess.Request{AccountID: owner, UserID: owner, CLIClientSessionID: owner, OperationID: "policy-regrant-test", ResourceKind: "machine_service", ResourceID: machine, RouteID: "tcp:80", Protocol: "tcp"}
	oldGrant, err := resolver.ResolveNativePrivate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.MachineServiceCurrent(ctx, request, oldGrant.AccessSessionID, oldGrant); err != nil {
		t.Fatalf("initial grant: %v", err)
	}
	if err = svc.Delete(ctx, owner, first.ID, first.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.ResolveNativePrivate(ctx, request); !errors.Is(err, nativeprivateaccess.ErrDenied) {
		t.Fatalf("revoked port: %v", err)
	}
	second, err := svc.Upsert(ctx, owner, input)
	if err != nil {
		t.Fatalf("regrant revoked port: %v", err)
	}
	if second.ID == first.ID || second.Hostname != first.Hostname || second.Generation != 1 {
		t.Fatalf("new identity/stable hostname: first=%+v second=%+v", first, second)
	}
	if _, err = svc.Upsert(ctx, owner, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate active grant: %v", err)
	}
	if _, err = svc.Get(ctx, owner, first.ID); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked policy revived: %v", err)
	}
	if _, err = resolver.MachineServiceCurrent(ctx, request, oldGrant.AccessSessionID, oldGrant); !errors.Is(err, nativeprivateaccess.ErrDenied) {
		t.Fatalf("old grant revived: %v", err)
	}
	freshGrant, err := resolver.ResolveNativePrivate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if freshGrant.AccessSessionID == oldGrant.AccessSessionID {
		t.Fatal("regrant reused revoked access session")
	}
	if _, err = resolver.MachineServiceCurrent(ctx, request, freshGrant.AccessSessionID, freshGrant); err != nil {
		t.Fatalf("fresh grant: %v", err)
	}
	if err = svc.Delete(ctx, owner, second.ID, second.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.MachineServiceCurrent(ctx, request, freshGrant.AccessSessionID, freshGrant); !errors.Is(err, nativeprivateaccess.ErrDenied) {
		t.Fatalf("second withdrawal: %v", err)
	}
	third, err := svc.Upsert(ctx, owner, input)
	if err != nil || third.ID == first.ID || third.ID == second.ID || third.Hostname != first.Hostname {
		t.Fatalf("repeated regrant: %+v %v", third, err)
	}
	if err = svc.Delete(ctx, owner, third.ID, third.Generation); err != nil {
		t.Fatal(err)
	}
	var tombstones, reservations int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.lazy_access_policies WHERE machine_id=$1 AND deleted_at IS NOT NULL`, machine).Scan(&tombstones); err != nil || tombstones != 3 {
		t.Fatalf("retained history: %d %v", tombstones, err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.managed_hostnames WHERE resource_kind='machine' AND resource_id=$1 AND port=80 AND hostname=$2`, machine, first.Hostname).Scan(&reservations); err != nil || reservations != 1 {
		t.Fatalf("stable owner reservation: %d %v", reservations, err)
	}
}
